#!/usr/bin/env bash
# Change the configuration of the aks cluster
# 1  = oci images without nydus or cluster cache
# 2  = nydus without cluster cache, every read still goes through the chunk proxy
# 2s = stock nydusd without cluster cache, nydusd reads the registry itself and no proxy is involved
# 3  = dragonfly
# 4  = nydus with cluster cache
# Under 2s, PREFETCH=all makes nydusd fetch the whole image ahead of the first read
# and NYDUS_TAG picks the image conversion (nydus-none unless set)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:?usage: ACR=<acr> [PREFETCH=off|all] [NYDUS_TAG=nydus-none] config-switch-aks.sh <1|2|2s|3|4>}"
ACR="${ACR:?set ACR to the ACR login server}"
PREFETCH="${PREFETCH:-off}"
NYDUS_TAG="${NYDUS_TAG:-nydus-none}"

# Only stock nydusd can prefetch or read compressed chunks here, the chunk
# proxy path is built for the uncompressed conversion alone
if [[ "${CONFIG}" != "2s" && ( "${PREFETCH}" != "off" || "${NYDUS_TAG}" != "nydus-none" ) ]]; then
  echo "PREFETCH=${PREFETCH} NYDUS_TAG=${NYDUS_TAG} only apply to config 2s" >&2
  exit 1
fi

# run a shell cmd on every node
nodeexec_all() {
  local pod
  for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o name); do
    kubectl -n kube-system exec "${pod#pod/}" -- nsenter -t 1 -m -p -n -- sh -c "$1"
  done
}

# Enable/disable the nydus snapshotter by modifying the containerd config file
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

  sleep 10
  kubectl wait --for=condition=Ready nodes --all --timeout=180s
  nodeexec_all "crictl info 2>/dev/null | grep -o '\"snapshotter\": \"[a-z]*\"' | head -1"
}

# Enable/disable the cluster cache route via the corresponding chunk proxy input param
set_cache_enabled() {
  kubectl -n kube-system patch ds/chunk-proxy --type=json -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/1\",\"value\":\"--cache-enabled=$1\"}]"
  kubectl -n kube-system rollout status ds/chunk-proxy --timeout=300s
}

# Check that the dragonfly client responds on every cluster node
# => if not we need to stop as this would pollute results because nydus would silently fetch from the remote registry...
check_dragonfly() {
  nodeexec_all "curl -sf -m 5 http://127.0.0.1:4003/healthy >/dev/null" || { echo "dragonfly daemon is not answering on every node" >&2; exit 1; }
}

# Remote the python test service
remove_workload() {
  kubectl delete ksvc "${SERVICE:-fn-python-hello}" --ignore-not-found --timeout=120s
  kubectl wait --for=delete pod -l "serving.knative.dev/service=${SERVICE:-fn-python-hello}" --timeout=120s 2>/dev/null || true
}

# Copy the nydusd config (which contains where to download chunks from) to every node
# c2 & c4 = points to chunk proxy on the same node
# c2s = stock nydusd reading the registry named in the image reference, with or without prefetch
# c3 = points to dragonfly client on the same node (which in turn fetches from other nodes via p2p or falls back to registry)
set_nydusd_backend() {
  local variant="$1" rendered encoded pod verdict
  if [[ "${variant}" == "dragonfly" ]]; then
    rendered="$(sed "s|__REGISTRY__|https://${ACR}|" "${REPO_ROOT}/deploy/aks/installer/nydusd-config-dragonfly.json")"
  elif [[ "${variant}" == "stock" && "${PREFETCH}" == "all" ]]; then
    rendered="$(cat "${REPO_ROOT}/deploy/aks/installer/nydusd-config-stock-prefetch.json")"
  elif [[ "${variant}" == "stock" && "${PREFETCH}" == "off" ]]; then
    rendered="$(cat "${REPO_ROOT}/deploy/aks/installer/nydusd-config-stock.json")"
  elif [[ "${variant}" == "stock" ]]; then
    echo "unknown prefetch mode ${PREFETCH}; choose off or all" >&2
    exit 1
  else
    rendered="$(cat "${REPO_ROOT}/deploy/aks/installer/nydusd-config.json")"
  fi
  encoded="$(printf '%s' "${rendered}" | base64 -w0)"

  for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o name); do
    # Run the copy from the view of the node itself
    verdict="$(kubectl -n kube-system exec "${pod#pod/}" -- nsenter -t 1 -m -p -- sh -c "
      echo '${encoded}' | base64 -d >/etc/nydus/nydusd-config.wanted
      # The new config is the same as the currently deployed one so nothing to do...
      if cmp -s /etc/nydus/nydusd-config.wanted /etc/nydus/nydusd-config.json; then
        rm -f /etc/nydus/nydusd-config.wanted
        echo template-unchanged
      # Replace old nydusd config with the new one
      else
        mv /etc/nydus/nydusd-config.wanted /etc/nydus/nydusd-config.json
        echo template-replaced
      fi
    " | tail -1)"
    [[ "${verdict}" == "template-unchanged" ]] && continue

    # Restart nydus on every node where we previously replaces the config file so it gets read again
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
      echo "nydus-snapshotter doesnt respond after restarting" >&2
      exit 1
    '
  done
}

# Fail the switch when a node does not carry the backend and prefetch mode the
# config asked for. Stock nydusd and Dragonfly share the registry type and
# differ only in the mirror, so the mirror count is part of what is checked
assert_backend() {
  local backend="$1" mirrors="$2" prefetch="$3" pod verdict
  for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o name); do
    if ! verdict="$(kubectl -n kube-system exec "${pod#pod/}" -- nsenter -t 1 -m -p -- \
        cat /etc/nydus/nydusd-config.json \
        | python3 "${REPO_ROOT}/scripts/check-nydusd-config.py" "${backend}" "${mirrors}" "${prefetch}")"; then
      echo "${pod#pod/}: ${verdict}" >&2
      exit 1
    fi
  done
  echo "nydusd on every node: ${verdict}"
}

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
  return 1
}

case "${CONFIG}" in
  1)
    echo "Config 1: plain containerd with oci images"
    set_snapshotter overlayfs
    apply_workload "oci"
    ;;
  2)
    echo "Config 2: nydus without cluster cache"
    set_snapshotter nydus
    set_cache_enabled "false"
    remove_workload
    set_nydusd_backend http-proxy
    assert_backend http-proxy 0 off
    apply_workload "nydus-none"
    ;;
  2s)
    echo "Config 2s: stock nydusd reading the registry itself, prefetch ${PREFETCH}, tag ${NYDUS_TAG}"
    set_snapshotter nydus
    set_cache_enabled "false"
    remove_workload
    set_nydusd_backend stock
    assert_backend registry 0 "${PREFETCH}"
    apply_workload "${NYDUS_TAG}"
    ;;
  4)
    echo "Config 4: nydus + cluster cache"
    set_snapshotter nydus
    set_cache_enabled "true"
    remove_workload
    set_nydusd_backend http-proxy
    assert_backend http-proxy 0 off
    apply_workload "nydus-none"
    ;;
  3)
    echo "Config 3: nydus + Dragonfly p2p"
    check_dragonfly
    set_snapshotter nydus
    set_cache_enabled "false"
    remove_workload
    set_nydusd_backend dragonfly
    assert_backend registry 1 off
    apply_workload "nydus-none"
    ;;
  *)
    echo "Unknown config ${CONFIG}" >&2
    exit 1
    ;;
esac
