#!/bin/sh
set -eu

# Converges one AKS node: nydus binaries + configs + systemd unit + containerd
# proxy_plugins registration. Idempotent — safe on every DaemonSet (re)start,
# which also heals a reimaged node. Runs in the HOST mount namespace; the
# DaemonSet stages the payload at /opt/nydus-installer first.
PAYLOAD=/opt/nydus-installer

install -m 0755 ${PAYLOAD}/containerd-nydus-grpc ${PAYLOAD}/nydusd ${PAYLOAD}/nydus-image /usr/local/bin/
mkdir -p /etc/nydus /var/lib/containerd-nydus
cp ${PAYLOAD}/config.toml /etc/nydus/config.toml
cp ${PAYLOAD}/nydusd-config.json /etc/nydus/nydusd-config.json
cp ${PAYLOAD}/nydus-snapshotter.service /etc/systemd/system/nydus-snapshotter.service
systemctl daemon-reload
systemctl enable --now nydus-snapshotter

# Register the proxy plugin with containerd exactly once. The plugin being
# registered is harmless while the CRI default stays overlayfs; the CRI
# default itself is config-switch-aks.sh's job, not the installer's.
# RUN-TIME NOTE: if the AKS config already carries a [proxy_plugins] table,
# nest only the sub-table instead of appending a second header (duplicate
# top-level tables are a TOML error).
if ! grep -q 'proxy_plugins.nydus' /etc/containerd/config.toml; then
  cat >>/etc/containerd/config.toml <<'EOF'

[proxy_plugins]
  [proxy_plugins.nydus]
    type = "snapshot"
    address = "/run/containerd-nydus/containerd-nydus-grpc.sock"
EOF
  systemctl restart containerd
fi
echo "node converged"
