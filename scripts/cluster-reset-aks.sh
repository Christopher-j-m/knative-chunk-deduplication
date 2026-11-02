#!/usr/bin/env bash
# Resets a aks cluster to cold state after benchmark run
set -euo pipefail

ACR="${ACR:?set ACR to the ACR login server}"
SERVICE="${SERVICE:-fn-python-hello}"
WORKLOAD_REPO="${WORKLOAD_REPO:-${ACR}/${SERVICE}}"

# Runs a shell cmd on a specific aks node
nodeexec() {
  kubectl -n kube-system exec "$1" -- nsenter -t 1 -m -p -- sh -c "$2"
}

# Every function service goes, not only the one under test: a container that
# still runs an image keeps its unpacked layers alive after the image record is
# removed, and corpus images share base layers, so any survivor would shorten
# the next eager pull of a different image.
#
# The function containers ignore the polite stop signal, and Knative gives
# them a grace period of five minutes that the node will not shorten once it
# has begun, so their pod sandboxes are stopped on the nodes directly
echo "Removing workloads from all nodes..."
kubectl delete ksvc --all -n default --wait=false >/dev/null
for attempt in $(seq 1 60); do
  # A service deleted moments after it was created can still spawn a pod, so
  # the services and their deployments must be gone as well as their pods
  if [[ -z "$(kubectl get ksvc -n default -o name 2>/dev/null)" \
        && -z "$(kubectl get deploy,pods -n default -l serving.knative.dev/service -o name 2>/dev/null)" ]]; then
    break
  fi
  if (( attempt == 60 )); then
    echo "function pods are still terminating, so their images cannot be removed" >&2
    exit 1
  fi
  for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o jsonpath='{.items[*].metadata.name}'); do
    nodeexec "${pod}" '
      for sandbox in $(crictl pods -q --namespace default --name "^fn-" 2>/dev/null); do
        crictl stopp "${sandbox}" >/dev/null 2>&1 || true
      done
    ' 2>/dev/null || true
  done
  sleep 3
done

echo "Restarting cache-service to clear in-memory"
kubectl rollout restart deploy/nydus-cluster-cache
kubectl rollout status deploy/nydus-cluster-cache --timeout=120s

echo "Deleting all images and clearing local nydus caches"
for pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o jsonpath='{.items[*].metadata.name}'); do
  nodeexec "${pod}" '
    for m in /var/lib/containerd-nydus/snapshots/*/mnt; do
      umount -f "${m}" 2>/dev/null || true
    done
    pkill -x nydusd 2>/dev/null || true
  ' || true

  # A deleted pod can leave its stopped sandbox on the node for minutes, and
  # that sandbox still holds the image's layers, so stopped sandboxes go first
  nodeexec "${pod}" '
    for sandbox in $(crictl pods -q --namespace default --state NotReady 2>/dev/null); do
      crictl rmp -f "${sandbox}" >/dev/null 2>&1 || true
    done
  ' || true

  # Remove all dummy images by their id
  for _ in $(seq 1 12); do
    ids="$(nodeexec "${pod}" "crictl images 2>/dev/null" | awk -v corpus="${ACR}/fn-" 'index($1, corpus) == 1 {print $3}' | sort -u)"
    [[ -z "${ids}" ]] && break
    for id in ${ids}; do
      nodeexec "${pod}" "crictl rmi ${id}" >/dev/null 2>&1 || true
    done
    sleep 5
  done

  # Unpacked layers that outlive their image are removed outright, newest
  # first since a layer cannot go while one built on it remains. Containerd
  # would collect them eventually, but a pull that starts before it does
  # finds them and skips the download
  if [[ -n "${IMAGE_FACTS:-}" && -s "${IMAGE_FACTS}" ]]; then
    chains="$(python3 -c 'import json,sys
facts = json.load(open(sys.argv[1]))
for image in facts.values():
    for tag in image.values():
        print(" ".join(reversed(tag["chain_ids"])))' "${IMAGE_FACTS}")"
    nodeexec "${pod}" "
      while read -r chain; do
        for name in \${chain}; do
          for snapshotter in overlayfs nydus; do
            ctr -n k8s.io snapshots --snapshotter \${snapshotter} rm \${name} >/dev/null 2>&1 || true
          done
        done
      done <<'CHAINS'
${chains}
CHAINS
    " || true
  fi

  # Stop the nydus snapshotter and remove its cache before restarting
  nodeexec "${pod}" '
    systemctl stop nydus-snapshotter
    pkill -x nydusd 2>/dev/null || true
    sleep 1
    rm -rf /var/lib/containerd-nydus/cache/* 2>/dev/null || true
    systemctl start nydus-snapshotter
  ' || true
  
  nodeexec "${pod}" '
    for _ in $(seq 1 90); do
      ctr -n k8s.io snapshots --snapshotter nydus ls >/dev/null 2>&1 && exit 0
      sleep 2
    done
    echo "nydus-snapshotter not responding after restart" >&2
    exit 1
  '
done

# Restart dragonfly clients (also in-memory) to let them forget all downloaded chunks
if [[ "${RESET_DRAGONFLY:-0}" == "1" ]]; then
  kubectl -n dragonfly-system rollout restart ds/dragonfly-client
  kubectl -n dragonfly-system rollout status ds/dragonfly-client --timeout=300s

  # Start and wait until a test pod starts under 4s on all nodes
  # The idea is that node needs after dragonfly restart a bit to spin up new pods
  # which will get faster after some time, which we wait here until they only take maximum 4s
  for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    canary="settle-canary-${node##*-}" # name of this nodes test pod
    
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
        echo "Pod creation on ${node} finished under 4s, done with this node..."
        break
      fi
      echo "Pod creation on ${node} still too slow after the restart (${canary_elapsed}s)..."
      sleep 20
    done
  done
fi

# Check if the chunk proxy is still healthy
kubectl -n kube-system rollout status ds/chunk-proxy --timeout=180s
