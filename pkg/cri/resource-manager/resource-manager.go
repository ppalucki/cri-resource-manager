// Copyright 2019 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resmgr

import (
	"golang.org/x/sys/unix"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pkgcfg "github.com/intel/cri-resource-manager/pkg/config"
	"github.com/intel/cri-resource-manager/pkg/cri/relay"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/agent"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/cache"
	config "github.com/intel/cri-resource-manager/pkg/cri/resource-manager/config"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/control"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/introspect"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/metrics"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/policy"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/visualizer"
	"github.com/intel/cri-resource-manager/pkg/instrumentation"
	logger "github.com/intel/cri-resource-manager/pkg/log"
	"github.com/intel/cri-resource-manager/pkg/utils"
)

// ResourceManager is the interface we expose for controlling the CRI resource manager.
type ResourceManager interface {
	// Start starts the resource manager.
	Start() error
	// Stop stops the resource manager.
	Stop()
	// SetConfig dynamically updates the resource manager  configuration
	SetConfig(*config.RawConfig) error
	// SendEvent sends an event to be processed by the resource manager.
	SendEvent(event interface{}) error
	// Add-ons for testing.
	ResourceManagerTestAPI
}

// resmgr is the implementation of ResourceManager.
type resmgr struct {
	logger.Logger
	sync.Mutex
	relay        relay.Relay        // our CRI relay
	cache        cache.Cache        // cached state
	policy       policy.Policy      // resource manager policy
	configServer config.Server      // configuration management server
	control      control.Control    // policy controllers/enforcement
	agent        agent.Interface    // connection to cri-resmgr agent
	conf         *config.RawConfig  // pending for saving in cache
	metrics      *metrics.Metrics   // metrics collector/pre-processor
	events       chan interface{}   // channel for delivering events
	stop         chan interface{}   // channel for signalling shutdown to goroutines
	signals      chan os.Signal     // signal channel
	introspect   *introspect.Server // server for external introspection
}

// NewResourceManager creates a new ResourceManager instance.
func NewResourceManager() (ResourceManager, error) {
	m := &resmgr{Logger: logger.NewLogger("resource-manager")}

	if err := m.setupCache(); err != nil {
		return nil, err
	}

	if opt.ResetPolicy {
		os.Exit(m.resetCachedPolicy())
	}

	if err := m.checkOpts(); err != nil {
		return nil, err
	}

	if opt.ForceConfig == "" {
		if err := m.setupConfigAgent(); err != nil {
			return nil, err
		}
	}

	if err := m.loadConfig(); err != nil {
		return nil, err
	}

	if err := m.setupConfigServer(); err != nil {
		return nil, err
	}

	if err := m.setupPolicy(); err != nil {
		return nil, err
	}

	if err := m.setupRelay(); err != nil {
		return nil, err
	}

	if err := m.setupRequestProcessing(); err != nil {
		return nil, err
	}

	if err := m.setupEventProcessing(); err != nil {
		return nil, err
	}

	if err := m.setupControllers(); err != nil {
		return nil, err
	}

	if err := m.setupIntrospection(); err != nil {
		return nil, err
	}

	return m, nil
}

// Start starts the resource manager.
func (m *resmgr) Start() error {
	m.Info("starting...")

	m.Lock()
	defer m.Unlock()

	if err := m.startControllers(); err != nil {
		return err
	}

	if err := m.startRequestProcessing(); err != nil {
		return err
	}

	if err := m.startEventProcessing(); err != nil {
		return err
	}

	m.startIntrospection()

	if err := m.relay.Start(); err != nil {
		return resmgrError("failed to start CRI relay: %v", err)
	}

	if opt.ForceConfig == "" {
		if err := m.configServer.Start(opt.ConfigSocket); err != nil {
			return resmgrError("failed to start configuration server: %v", err)
		}

		// We never store a forced configuration in the cache. However, if we're not
		// running with a forced configuration, and the configuration is pending to
		// get stored in the cache (IOW, it is a new one acquired from an agent), then
		// then store it in the cache now.
		if m.conf != nil {
			m.cache.SetConfig(m.conf)
			m.conf = nil
		}
	}

	m.Info("up and running")

	return nil
}

// Stop stops the resource manager.
func (m *resmgr) Stop() {
	m.Info("shutting down...")

	m.Lock()
	defer m.Unlock()

	if m.signals != nil {
		close(m.signals)
		m.signals = nil
	}

	m.configServer.Stop()
	m.relay.Stop()
	m.stopIntrospection()
	m.stopEventProcessing()
}

// SetConfig pushes new configuration to the resource manager.
func (m *resmgr) SetConfig(conf *config.RawConfig) error {
	m.Info("applying new configuration from agent...")

	m.Lock()
	defer m.Unlock()
	return m.setConfig(conf)
}

// setConfigFromFile pushes new configuration to the resource manager from a file.
func (m *resmgr) setConfigFromFile(path string) error {
	m.Info("applying new configuration from file %s...", path)

	m.Lock()
	defer m.Unlock()
	return m.setConfig(path)
}

// setConfig pushes new configuration from the agent or a file.
func (m *resmgr) setConfig(src interface{}) error {
	var pending *config.RawConfig
	var err error

	switch cfg := src.(type) {
	case *config.RawConfig:
		pending = cfg // schedule for storing in the cache
		err = pkgcfg.SetConfig(cfg.Data)
	case string:
		// never store a forced configuration in the cache
		err = pkgcfg.SetConfigFromFile(cfg)
	default:
		return resmgrError("configuration source of invalid type %T", src)
	}

	if err != nil {
		m.Error("configuration was rejected: %v", err)
		return resmgrError("configuration rejected: %v", err)
	}

	// TODO: save current configuration and roll back if some controllers fail to start

	if err := m.control.StartStopControllers(m.cache, m.relay.Client()); err != nil {
		m.Error("failed to activate new configuration: %v", err)
		return resmgrError("failed to fully activate configuration: %v", err)
	}

	// If the update was not from a forced configuration (IOW it was from the
	// agent) and the update was activated successfully, then store it in the
	// cache now.
	if pending != nil {
		m.cache.SetConfig(pending)
	}

	m.Info("sucessfully switched to new configuration")
	return nil
}

// resetCachedPolicy resets the cached active policy and all of its data.
func (m *resmgr) resetCachedPolicy() int {
	defer logger.Flush()

	if utils.ServerActiveAt(opt.RelaySocket) {
		m.Error("Refusing to reset active policy from cache.")
		m.Error("Looks like an instance of %q is active at socket %q...",
			filepath.Base(os.Args[0]), opt.RelaySocket)
		return 1
	}

	if err := m.cache.ResetActivePolicy(); err != nil {
		m.Error("failed to reset active policy: %v", err)
		return 1
	}
	return 0
}

// setupCache creates a cache and reloads its last saved state if found.
func (m *resmgr) setupCache() error {
	var err error

	options := cache.Options{CacheDir: opt.RelayDir}
	if m.cache, err = cache.NewCache(options); err != nil {
		return resmgrError("failed to create cache: %v", err)
	}

	return nil

}

// setupConfigAgent sets up the connection to the configuration agent.
func (m *resmgr) setupConfigAgent() error {
	var err error

	if m.agent, err = agent.NewAgentInterface(opt.AgentSocket); err != nil {
		return err
	}

	return nil
}

// setupConfigServer sets up our configuration server for agent notifications.
func (m *resmgr) setupConfigServer() error {
	var err error

	if m.configServer, err = config.NewConfigServer(m.SetConfig); err != nil {
		return resmgrError("failed to create configuration notification server: %v", err)
	}

	return nil
}

// checkOpts checks the command line options for obvious errors.
func (m *resmgr) checkOpts() error {
	if opt.ForceConfig != "" && opt.FallbackConfig != "" {
		return resmgrError("both fallback (%s) and forced (%s) configurations given",
			opt.FallbackConfig, opt.ForceConfig)
	}

	return nil
}

// loadConfig tries to pick and load (initial) configuration from a number of sources.
func (m *resmgr) loadConfig() error {
	//
	// We try to load initial configuration from a number of sources:
	//
	//    1. use forced configuration file if we were given one
	//    2. use configuration from agent, if we can fetch it and it applies
	//    3. use last configuration stored in cache, if we have one and it applies
	//    4. use fallback configuration file if we were given one
	//    5. use empty/builtin default configuration, whatever that is...
	//
	// Notes/TODO:
	//   If the agent is already running at this point, the initial configuration is
	//   obtained by polling the agent via GetConfig(). Unlike for the latter updates
	//   which are pushed by the agent, there is currently no way to report problems
	//   about polled configuration back to the agent. If/once the agent will have a
	//   mechanism to propagate configuration errors back to the origin, this might
	//   become a problem that we'll need to solve.
	//

	if opt.ForceConfig != "" {
		m.Info("using forced configuration %s...", opt.ForceConfig)
		if err := pkgcfg.SetConfigFromFile(opt.ForceConfig); err != nil {
			return resmgrError("failed to load forced configuration %s: %v",
				opt.ForceConfig, err)
		}
		return m.setupConfigSignal(opt.ForceConfigSignal)
	}

	m.Info("trying configuration from agent...")
	if conf, err := m.agent.GetConfig(1 * time.Second); err == nil {
		if err = pkgcfg.SetConfig(conf.Data); err == nil {
			m.conf = conf // schedule storing in cache if we ever manage to start up
			return nil
		}
		m.Error("configuration from agent failed to apply: %v", err)
	}

	m.Info("trying last cached configuration...")
	if conf := m.cache.GetConfig(); conf != nil {
		err := pkgcfg.SetConfig(conf.Data)
		if err == nil {
			return nil
		}
		m.Error("failed to activate cached configuration: %v", err)
	}

	if opt.FallbackConfig != "" {
		m.Info("using fallback configuration %s...", opt.FallbackConfig)
		if err := pkgcfg.SetConfigFromFile(opt.FallbackConfig); err != nil {
			return resmgrError("failed to load fallback configuration %s: %v",
				opt.FallbackConfig, err)
		}
		return nil
	}

	m.Warn("no initial configuration found")
	return nil
}

// setupConfigSignal sets up a signal handler for reloading forced configuration.
func (m *resmgr) setupConfigSignal(signame string) error {
	if signame == "" || strings.HasPrefix(strings.ToLower(signame), "disable") {
		return nil
	}

	m.Info("setting up signal %s to reload forced configuration", signame)

	sig := unix.SignalNum(signame)
	if int(sig) == 0 {
		return resmgrError("invalid forced configuration reload signal '%s'", signame)
	}

	m.signals = make(chan os.Signal, 1)
	signal.Notify(m.signals, sig)

	go func(signals <-chan os.Signal) {
		for {
			select {
			case _, ok := <-signals:
				if !ok {
					return
				}
			}

			m.Info("reloading forced configuration %s...", opt.ForceConfig)

			if err := m.setConfigFromFile(opt.ForceConfig); err != nil {
				m.Error("failed to reload forced configuration %s: %v",
					opt.ForceConfig, err)
			}
		}
	}(m.signals)

	return nil
}

// setupPolicy sets up policy with the configured/active backend
func (m *resmgr) setupPolicy() error {
	var err error

	active := policy.ActivePolicy()
	cached := m.cache.GetActivePolicy()

	if active != cached {
		if cached != "" {
			if !opt.AllowPolicySwitch {
				return resmgrError("cannot load cache with policy %s for active policy %s",
					cached, active)
			}
			if err := m.cache.ResetActivePolicy(); err != nil {
				return resmgrError("failed to reset cached policy %q: %v", cached, err)
			}
		}
		m.cache.SetActivePolicy(active)
	}

	options := &policy.Options{AgentCli: m.agent, SendEvent: m.SendEvent}
	if m.policy, err = policy.NewPolicy(m.cache, options); err != nil {
		return resmgrError("failed to create policy %s: %v", active, err)
	}

	return nil
}

// setupRelay sets up the CRI request relay.
func (m *resmgr) setupRelay() error {
	var err error

	options := relay.Options{
		RelaySocket:   opt.RelaySocket,
		ImageSocket:   opt.ImageSocket,
		RuntimeSocket: opt.RuntimeSocket,
	}
	if m.relay, err = relay.NewRelay(options); err != nil {
		return resmgrError("failed to create CRI relay: %v", err)
	}

	if err = m.relay.Setup(); err != nil {
		return resmgrError("failed to create CRI relay: %v", err)
	}

	return nil
}

// setupControllers sets up the resource controllers.
func (m *resmgr) setupControllers() error {
	var err error

	if m.control, err = control.NewControl(); err != nil {
		return resmgrError("failed to create resource controller: %v", err)
	}

	return nil
}

// startControllers start the resource controllers.
func (m *resmgr) startControllers() error {
	if err := m.control.StartStopControllers(m.cache, m.relay.Client()); err != nil {
		return resmgrError("failed to start resource controllers: %v", err)
	}

	return nil
}

// setupIntrospection prepares the resource manager for serving external introspection requests.
func (m *resmgr) setupIntrospection() error {
	mux := instrumentation.GetHTTPMux()

	i, err := introspect.Setup(mux, m.policy.Introspect())
	if err != nil {
		return resmgrError("failed to set up introspection service: %v", err)
	}
	m.introspect = i

	if !opt.DisableUI {
		if err := visualizer.Setup(mux); err != nil {
			m.Error("failed to set up UI for visualization: %v", err)
		}
	} else {
		m.Warn("built-in visualization UIs are disabled")
	}

	return nil
}

// startIntrospection starts serving the external introspection requests.
func (m *resmgr) startIntrospection() {
	m.introspect.Start()
	m.updateIntrospection()
}

// stopInstrospection stops serving external introspection requests.
func (m *resmgr) stopIntrospection() {
	m.introspect.Stop()
}

// updateIntrospection pushes updated data for external introspection·
func (m *resmgr) updateIntrospection() {
	m.introspect.Set(m.policy.Introspect())
}
