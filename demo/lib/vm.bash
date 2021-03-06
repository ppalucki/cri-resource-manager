source "$(dirname "${BASH_SOURCE[0]}")/command.bash"

VM_PROMPT=${VM_PROMPT-"\e[38;5;11mroot@vm>\e[0m "}
VM_SSH_USER=ubuntu
VM_IMAGE_URL=https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img
VM_IMAGE=$(basename "$VM_IMAGE_URL")

vm-command() {
    command-start "vm" "$VM_PROMPT" "$1"
    if [ "$2" == "bg" ]; then
        ( ssh -o StrictHostKeyChecking=No ${VM_SSH_USER}@${VM_IP} sudo bash <<<"$COMMAND" 2>&1 | command-handle-output ;
          command-end ${PIPESTATUS[0]}
        ) &
        command-runs-in-bg
    else
        ssh -o StrictHostKeyChecking=No ${VM_SSH_USER}@${VM_IP} sudo bash <<<"$COMMAND" 2>&1 | command-handle-output ;
        command-end ${PIPESTATUS[0]}
    fi
    return $COMMAND_STATUS
}

vm-command-q() {
    ssh -o StrictHostKeyChecking=No ${VM_SSH_USER}@${VM_IP} sudo bash <<<"$1"
}

vm-wait-process() {
    # parameters: "process" and "timeout" (optional, default 30 seconds)
    process=$1
    timeout=${2-30}
    if ! vm-command-q "retry=$timeout; until ps -A | grep -q $process; do retry=\$(( \$retry - 1 )); [ \"\$retry\" == \"0\" ] && exit 1; sleep 1; done"; then
        error "waiting for process \"$process\" timed out"
    fi
}

vm-networking() {
    vm-command-q "grep -q 1 /proc/sys/net/ipv4/ip_forward" || vm-command "sysctl -w net.ipv4.ip_forward=1"
    vm-command-q "grep -q ^net.ipv4.ip_forward=1 /etc/sysctl.conf" || vm-command "sed -i 's/#net.ipv4.ip_forward=1/net.ipv4.ip_forward=1/g' /etc/sysctl.conf"
    vm-command-q "grep -q 1 /proc/sys/net/bridge/bridge-nf-call-iptables 2>/dev/null" || {
        vm-command "modprobe br_netfilter"
        vm-command "echo br_netfilter > /etc/modules-load.d/br_netfilter.conf"
    }
    vm-command-q "grep -q \$(hostname) /etc/hosts" || vm-command "echo \"$VM_IP \$(hostname)\" >/etc/hosts"
    if [ -n "$http_proxy" ] || [ -n "$https_proxy" ] || [ -n "$no_proxy" ]; then
        vm-command-q "grep -q no_proxy /etc/environment" || vm-command "(echo http_proxy=$http_proxy; echo https_proxy=$https_proxy; echo no_proxy=$no_proxy,$VM_IP,10.96.0.0/12,10.217.0.0/16,\$(hostname) ) >> /etc/environment"
    fi
}

vm-install-containerd() {
    vm-command-q "[ -f /usr/bin/containerd ]" || {
        vm-command "apt install -y containerd"
        # Set proxy environment to containers managed by containerd, if needed.
        if [ -n "$http_proxy" ] || [ -n "$https_proxy" ] || [ -n "$no_proxy" ]; then
            speed=120 vm-command "mkdir -p /etc/systemd/system/containerd.service.d; (echo '[Service]'; echo 'Environment=HTTP_PROXY=$http_proxy'; echo 'Environment=HTTPS_PROXY=$https_proxy'; echo \"Environment=NO_PROXY=$no_proxy,$VM_IP,10.96.0.0/12,10.217.0.0/16,\$(hostname)\" ) > /etc/systemd/system/containerd.service.d/proxy.conf; systemctl daemon-reload; systemctl restart containerd"
        fi
    }
}

vm-install-containernetworking() {
    vm-command-q "command -v go >/dev/null" || vm-command "apt install -y golang"
    vm-command "go get -d github.com/containernetworking/plugins"
    CNI_PLUGINS_SOURCE_DIR=$(awk '/package.*plugins/{print $NF}' <<< $COMMAND_OUTPUT)
    [ -n "$CNI_PLUGINS_SOURCE_DIR" ] || {
        command-error "downloading containernetworking plugins failed"
    }
    vm-command "pushd \"$CNI_PLUGINS_SOURCE_DIR\" && ./build_linux.sh && mkdir -p /opt/cni && cp -rv bin /opt/cni && popd" || {
        command-error "building and installing cri-tools failed"
    }
    vm-command 'rm -rf /etc/cni/net.d && mkdir -p /etc/cni/net.d && cat > /etc/cni/net.d/10-bridge.conf <<EOF
{
  "cniVersion": "0.4.0",
  "name": "mynet",
  "type": "bridge",
  "bridge": "cni0",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.217.0.0/16",
    "routes": [
      { "dst": "0.0.0.0/0" }
    ]
  }
}
EOF'
    vm-command 'cat > /etc/cni/net.d/20-portmap.conf <<EOF
{
    "cniVersion": "0.4.0",
    "type": "portmap",
    "capabilities": {"portMappings": true},
    "snat": true
}
EOF'
    vm-command 'cat > /etc/cni/net.d/99-loopback.conf <<EOF
{
  "cniVersion": "0.4.0",
  "name": "lo",
  "type": "loopback"
}
EOF'
}

vm-install-k8s() {
    vm-command "apt update && apt install -y apt-transport-https curl"
    speed=60 vm-command "curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key add -"
    speed=60 vm-command "echo \"deb https://apt.kubernetes.io/ kubernetes-xenial main\" > /etc/apt/sources.list.d/kubernetes.list"
    vm-command "apt update &&  apt install -y kubelet kubeadm kubectl"
}

vm-print-usage() {
    echo "- Login VM:     ssh $VM_SSH_USER@$VM_IP"
    echo "- Stop VM:      govm stop $VM_NAME"
    echo "- Delete VM:    govm delete $VM_NAME"
}
