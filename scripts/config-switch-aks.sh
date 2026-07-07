#!/usr/bin/env bash
set -euo pipefail

# AKS twin of scripts/config-switch.sh. Same three knobs plus the nydusd
# backend template; the mechanism differs: the CRI snapshotter flips by
# editing /etc/containerd/config.toml on each node (through the
# nydus-node-setup DaemonSet + nsenter) and restarting containerd — real
# systemd nodes need no reboot, and the snapshotter is a systemd service
# that survives everything.
# Usage: ACR=<login-server> config-switch-aks.sh <1|2|3|4>
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:?usage: ACR=<acr> config-switch-aks.sh <1|2|3|4>}"
ACR="${ACR:?set ACR to the ACR login server}"

# Runs a shell command in the host namespaces of every node
nodeexec_all() {
  local pod
  for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o name); do
    kubectl -n kube-system exec "${pod#pod/}" -- nsenter -t 1 -m -p -n -- sh -c "$1"
  done
}

# The AKS containerd (2.x) config carries no explicit snapshotter line, so
# overlayfs is the implicit default. Nydus turns on by inserting an explicit
# snapshotter plus the annotation passthrough lazy pull needs right under the
# CRI images section, and turns off by deleting exactly those lines
set_snapshotter() {
  local want="$1"
  if [[ "${want}" == "nydus" ]]; then
    nodeexec_all "
      grep -q 'snapshotter = \"nydus\"' /etc/containerd/config.toml \
        || sed -i '/^\[plugins.\"io.containerd.cri.v1.images\"\]$/a\\  snapshotter = \"nydus\"\\n  disable_snapshot_annotations = false' /etc/containerd/config.toml
      systemctl restart containerd
    "
  else
    nodeexec_all "
      sed -i '/^  snapshotter = \"nydus\"$/d; /^  disable_snapshot_annotations = false$/d' /etc/containerd/config.toml
      systemctl restart containerd
    "
  fi
  # containerd restarting under the kubelet: give the runtime a moment, then
  # confirm every node reports the wanted snapshotter
  sleep 10
  kubectl wait --for=condition=Ready nodes --all --timeout=180s
  nodeexec_all "crictl info 2>/dev/null | grep -o '\"snapshotter\": \"[a-z]*\"' | head -1"
}

set_cache_enabled() {
  kubectl -n kube-system patch ds/chunk-proxy --type=json -p \
    "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/1\",\"value\":\"--cache-enabled=$1\"}]"
  kubectl -n kube-system rollout status ds/chunk-proxy --timeout=300s
}

# Refuses to switch into the Dragonfly configuration while any node lacks a
# reachable dfdaemon proxy, because nydusd would silently fall back to the
# plain registry and the measurement would stop involving Dragonfly at all
check_dragonfly() {
  nodeexec_all "curl -sf -m 5 http://127.0.0.1:4003/healthy >/dev/null" \
    || { echo "dfdaemon is not answering on every node; deploy Dragonfly first" >&2; exit 1; }
}

# Removes the benchmark service and waits for its pods to disappear so no
# nydusd holds a mount while the backend template swap bounces the
# snapshotter underneath it
remove_workload() {
  kubectl delete ksvc "${SERVICE:-fn-python-hello}" --ignore-not-found --timeout=120s
  kubectl wait --for=delete pod -l "serving.knative.dev/service=${SERVICE:-fn-python-hello}" --timeout=120s 2>/dev/null || true
}

# Pushes the wanted nydusd backend template to every node. On nodes where
# the content actually changed the snapshotter is restarted so daemons
# started from now on read the new backend; running daemons keep the old
# one, which is why the caller removes the workload first. The dragonfly
# variant points nydusd at the local dfdaemon proxy as a registry mirror
set_nydusd_backend() {
  local variant="$1" rendered encoded pod verdict
  if [[ "${variant}" == "dragonfly" ]]; then
    rendered="$(sed "s|__REGISTRY__|https://${ACR}|" "${REPO_ROOT}/deploy/aks/installer/nydusd-config-dragonfly.json")"
  else
    rendered="$(cat "${REPO_ROOT}/deploy/aks/installer/nydusd-config.json")"
  fi
  encoded="$(printf '%s' "${rendered}" | base64 -w0)"

  for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o name); do
    verdict="$(kubectl -n kube-system exec "${pod#pod/}" -- nsenter -t 1 -m -p -- sh -c "
      echo '${encoded}' | base64 -d >/etc/nydus/nydusd-config.wanted
      if cmp -s /etc/nydus/nydusd-config.wanted /etc/nydus/nydusd-config.json; then
        rm -f /etc/nydus/nydusd-config.wanted
        echo template-unchanged
      else
        mv /etc/nydus/nydusd-config.wanted /etc/nydus/nydusd-config.json
        echo template-replaced
      fi
    " | tail -1)"
    [[ "${verdict}" == "template-unchanged" ]] && continue

    # Same teardown-then-gate dance as the reset script: lingering daemons
    # would keep serving the old backend, and a pod scheduled before the
    # restarted snapshotter answers containerd sits in kubelet backoff
    kubectl -n kube-system exec "${pod#pod/}" -- nsenter -t 1 -m -p -- sh -c '
      for m in /var/lib/containerd-nydus/snapshots/*/mnt; do
        umount -f "${m}" 2>/dev/null || true
      done
      systemctl stop nydus-snapshotter
      pkill -x nydusd 2>/dev/null || true
      sleep 1
      systemctl start nydus-snapshotter
      for _ in $(seq 1 90); do
        ctr -n k8s.io snapshots --snapshotter nydus ls >/dev/null 2>&1 && exit 0
        sleep 2
      done
      echo "nydus-snapshotter still unresponsive after backend swap" >&2
      exit 1
    '
  done
}

# Replace the ksvc outright so no previous configuration's revision lingers;
# retried because the Knative webhook can lag a containerd restart. The
# manifest comes from the shared template so the switch smoke uses the same
# rendering path as the benchmarks
apply_workload() {
  local tag="$1" manifest
  manifest="$(mktemp)"
  ACR="${ACR}" SERVICE="${SERVICE:-fn-python-hello}" TAG="${tag}" PORT="${PORT:-8080}" \
    "${REPO_ROOT}/scripts/render-workload.sh" >"${manifest}"
  for _ in $(seq 1 30); do
    if kubectl delete ksvc "${SERVICE:-fn-python-hello}" --ignore-not-found --timeout=120s \
        && kubectl apply -f "${manifest}"; then
      rm -f "${manifest}"
      return 0
    fi
    sleep 5
  done
  rm -f "${manifest}"
  echo "apply_workload: Knative webhook never became reachable" >&2
  return 1
}

case "${CONFIG}" in
  1)
    echo "Config 1 - containerd baseline (overlayfs, OCI image)"
    set_snapshotter overlayfs
    apply_workload "oci"
    ;;
  2)
    echo "Config 2 - Nydus local-only (cache bypassed)"
    set_snapshotter nydus
    set_cache_enabled "false"
    remove_workload
    set_nydusd_backend http-proxy
    apply_workload "nydus-none"
    ;;
  4)
    echo "Config 4 - Nydus + cluster cache"
    set_snapshotter nydus
    set_cache_enabled "true"
    remove_workload
    set_nydusd_backend http-proxy
    apply_workload "nydus-none"
    ;;
  3)
    echo "Config 3 - Nydus + Dragonfly p2p (cluster cache not involved)"
    check_dragonfly
    set_snapshotter nydus
    set_cache_enabled "false"
    remove_workload
    set_nydusd_backend dragonfly
    apply_workload "nydus-none"
    ;;
  *)
    echo "Unknown config ${CONFIG}; choose 1, 2, 3, or 4" >&2
    exit 1
    ;;
esac
echo "Switched to config ${CONFIG}. Run scripts/cluster-reset-aks.sh before measuring."
