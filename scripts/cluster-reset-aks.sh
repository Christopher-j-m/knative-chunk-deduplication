#!/usr/bin/env bash
set -euo pipefail

# AKS twin of scripts/cluster-reset.sh: removes the workload, purges its image
# from every node, clears the in-memory cache and the node-side nydus
# blobcaches. Node access goes through the nydus-node-setup DaemonSet.
# The purge must go through crictl BY IMAGE ID: containerd keeps a bare
# image-id record (sha256:<config digest>) alongside the tag and digest
# names, so removing only the names that match the repository leaves the
# layers and snapshots referenced and the next "cold" pull finishes warm
# in a tenth of a second. Removing by id drops every name at once and
# frees the underlying content synchronously.
ACR="${ACR:?set ACR to the ACR login server}"
SERVICE="${SERVICE:-fn-python-hello}"
WORKLOAD_REPO="${WORKLOAD_REPO:-${ACR}/${SERVICE}}"

nodeexec() {
  kubectl -n kube-system exec "$1" -- nsenter -t 1 -m -p -- sh -c "$2"
}

echo "Removing the workload so its image can be purged from the nodes"
kubectl delete ksvc "${SERVICE}" --ignore-not-found --timeout=120s
kubectl wait --for=delete pod -l "serving.knative.dev/service=${SERVICE}" --timeout=120s 2>/dev/null || true

echo "Restarting cache-service to clear in-memory contents"
kubectl rollout restart deploy/nydus-cluster-cache
kubectl rollout status deploy/nydus-cluster-cache --timeout=120s

echo "Purging workload images and clearing node nydus caches"
for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o jsonpath='{.items[*].metadata.name}'); do
  # A live nydus mount keeps the whole snapshot chain busy, so removing the
  # image would leave the chain behind and the restarted snapshotter would
  # revive a daemon over a wiped cache that then serves nothing but read
  # errors. Unmounting and killing the daemons first lets the image removal
  # below take the entire chain with it
  nodeexec "${pod}" '
    for m in /var/lib/containerd-nydus/snapshots/*/mnt; do
      umount -f "${m}" 2>/dev/null || true
    done
    pkill -x nydusd 2>/dev/null || true
  ' || true
  # Purge every corpus image, not only the one being benchmarked: a chain
  # left behind by another image survives the blobcache wipe below and the
  # restarted snapshotter would revive its daemon over an empty cache,
  # which then fails every read on the next repetition. The fn- prefix
  # spares the chunk-proxy service image, which always runs on every node
  # and could never be removed anyway
  for _ in $(seq 1 12); do
    ids="$(nodeexec "${pod}" "crictl images 2>/dev/null" | awk -v corpus="${ACR}/fn-" 'index($1, corpus) == 1 {print $3}' | sort -u)"
    [[ -z "${ids}" ]] && break
    for id in ${ids}; do
      nodeexec "${pod}" "crictl rmi ${id}" >/dev/null 2>&1 || true
    done
    sleep 5
  done
  # The blobcache may only be wiped once no nydusd holds it open: systemd
  # alone can fail to kill a daemon behind a live FUSE mount (observed as
  # "Failed to kill control group: Invalid argument"), and a surviving
  # nydusd whose cache files vanished serves nothing but read errors
  nodeexec "${pod}" '
    systemctl stop nydus-snapshotter
    pkill -x nydusd 2>/dev/null || true
    sleep 1
    rm -rf /var/lib/containerd-nydus/cache/* 2>/dev/null || true
    systemctl start nydus-snapshotter
  ' || true
  # The snapshotter needs a moment after the restart before it answers
  # containerd again; a pod scheduled inside that window fails its sandbox
  # creation and sits in kubelet backoff for half a minute, which would
  # land squarely in the seed measurement
  nodeexec "${pod}" '
    for _ in $(seq 1 90); do
      ctr -n k8s.io snapshots --snapshotter nydus ls >/dev/null 2>&1 && exit 0
      sleep 2
    done
    echo "nydus-snapshotter still unresponsive after restart" >&2
    exit 1
  '
done

# A repetition must not inherit pieces the previous one already pulled
# through the p2p network: the Dragonfly clients keep their downloaded
# pieces on emptyDir volumes, so recycling the pods drops the disk caches.
# The scheduler deliberately stays up — its peer records die with the
# peers' gRPC connections, while restarting it leaves a growing trail of
# dead scheduler addresses in the manager database that every client then
# health-checks for 3 s apiece until the kubelet probe kills it (observed
# as a client crashloop after five resets). There is no seed peer to
# recycle: the seed role is disabled because it cannot authenticate
# back-to-source fetches against ACR. Safe at this point in the reset
# because every nydusd is already unmounted and killed above
if [[ "${RESET_DRAGONFLY:-0}" == "1" ]]; then
  echo "Recycling Dragonfly pods to drop the p2p disk caches"
  kubectl -n dragonfly-system rollout restart ds/dragonfly-client
  kubectl -n dragonfly-system rollout status ds/dragonfly-client --timeout=300s

  # The recycle churns a hostNetwork pod on every node and the kubelets
  # keep creating containers several seconds slower for a while afterwards,
  # which would land squarely in the next seed measurement (observed as a
  # uniform +6s on every pod, cold or warm). Gate on a canary pod starting
  # at nominal speed on each node before handing the cluster back; busybox
  # is already on the nodes as the Dragonfly init image, so the canary
  # never touches a registry and never warms anything the benchmark reads
  for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    canary="settle-canary-${node##*-}"
    for attempt in $(seq 1 10); do
      kubectl delete pod "${canary}" --ignore-not-found --timeout=60s >/dev/null 2>&1
      canary_start="$(date +%s.%N)"
      kubectl run "${canary}" --image=docker.io/library/busybox:latest \
        --image-pull-policy=IfNotPresent --restart=Never \
        --overrides="{\"spec\":{\"nodeName\":\"${node}\"}}" \
        --command -- sleep 3600 >/dev/null
      kubectl wait "pod/${canary}" --for=condition=Ready --timeout=120s >/dev/null
      canary_elapsed="$(awk -v a="$(date +%s.%N)" -v b="${canary_start}" 'BEGIN{printf "%.1f", a-b}')"
      kubectl delete pod "${canary}" --ignore-not-found --timeout=60s >/dev/null 2>&1
      if awk -v e="${canary_elapsed}" 'BEGIN{exit !(e < 4)}'; then
        echo "Pod creation on ${node} settled (canary ${canary_elapsed}s)"
        break
      fi
      echo "Pod creation on ${node} still slow after the recycle (canary ${canary_elapsed}s); waiting"
      sleep 20
    done
  done
fi

# The chunk-proxy deliberately keeps running through the reset: replacing
# the DaemonSet leaves each node without a listener for half a minute, and
# a daemon that mounts inside that window fails its first metadata fetch
# and serves nothing but read errors for the rest of its life. Leftover
# index entries for purged blobs are harmless because nothing asks for
# them again, and the liveness probe replaces a genuinely wedged proxy
kubectl -n kube-system rollout status ds/chunk-proxy --timeout=180s
echo "Reset complete"
