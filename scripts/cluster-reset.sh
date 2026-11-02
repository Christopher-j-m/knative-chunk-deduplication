#!/usr/bin/env bash
# Reset a cluster in a cold state with all services and images fully deleted (including the node local caches)
# Also restart the cluster cache and chunk proxy (since they only store in-memory)
set -euo pipefail

# Get all cluster nodes
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mapfile -t NODES < <("${REPO_ROOT}/scripts/cluster-nodes.sh")
if [[ "${#NODES[@]}" -eq 0 ]]; then
  echo "no cluster nodes found" >&2
  exit 1
fi
DROPIN=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.d/nydus-global.toml
CTR="ctr -a /run/k3s/containerd/containerd.sock -n k8s.io"
SERVICE="${SERVICE:-fn-python-hello}"

# The prefix that every dummy image starts with
# We'll delete all of them
CORPUS_PREFIX="${CORPUS_PREFIX:-registry.localhost:5000/fn-}"

CORPUS_POD_PREFIX="${CORPUS_PREFIX##*/}"
REGISTRY_CONTAINER=registry.localhost
REGISTRY_NETWORK=k3d-nydus-cache

# Set by func fetch_corpus_truth
CORPUS_EXPECTED_CONFIGS=""
CORPUS_EXPECTED_CHAINS=""

# Get every dummy image and its corresponding layers from the image registry
fetch_corpus_truth() {
  local registry_ip raw
  registry_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"${REGISTRY_NETWORK}\").IPAddress}}" "${REGISTRY_CONTAINER}")"
  raw="$(python3 - "${registry_ip}:5000" <<'PYEOF'
import sys, json, hashlib, urllib.request

# Registry address
registry = sys.argv[1]

# Send get request to the registry
def get(path, headers=None):
    request = urllib.request.Request(f"http://{registry}{path}", headers=headers or {})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read())

# Different kinds of response types that we may receive from the registry
# So far these were the only ones we received during testing
manifest_headers = {
    "Accept": ", ".join([
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    ])
}

# Returns the id of an image and the correpsonding layers
def chains_of(repo, tag):
    manifest = get(f"/v2/{repo}/manifests/{tag}", manifest_headers)
    if "manifests" in manifest:
        amd64 = next(
            (entry for entry in manifest["manifests"]
             if entry.get("platform", {}).get("architecture") == "amd64"),
            None,
        )
        if not amd64:
            return None, []
        manifest = get(
            f"/v2/{repo}/manifests/{amd64['digest']}",
            {"Accept": "application/vnd.oci.image.manifest.v1+json"},
        )

    # Get list of layers
    config_digest = manifest["config"]["digest"]
    config = get(f"/v2/{repo}/blobs/{config_digest}")
    diff_ids = config.get("rootfs", {}).get("diff_ids", [])
    if not diff_ids:
        return config_digest, []

    # Each layer has a different id based on its parent layer => same layer with diff parent have different id
    chains = [diff_ids[0]]
    for diff_id in diff_ids[1:]:
        chains.append("sha256:" + hashlib.sha256(f"{chains[-1]} {diff_id}".encode()).hexdigest())
    return config_digest, chains

# Stop if we cant reach the registry
try:
    catalog = get("/v2/_catalog")
except Exception as exc:
    print(f"could not reach registry at {registry}: {exc}", file=sys.stderr)
    sys.exit(1)

repos = [name for name in catalog.get("repositories", []) if name.startswith("fn-")]
if not repos:
    print("registry returned no dummy image repositories", file=sys.stderr)
    sys.exit(1)

# Get the layers of the cluster cache and chunk proxy since they need to be kept on the nodes
own_infrastructure_chains = set()
for repo in ("chunk-proxy", "nydus-cluster-cache"):
    try:
        for tag in get(f"/v2/{repo}/tags/list").get("tags") or []:
            _, chains = chains_of(repo, tag)
            own_infrastructure_chains.update(chains)
    except Exception as exc:
        print(f"could not resolve {repo}'s image chains: {exc}", file=sys.stderr)
        sys.exit(1)

# Print out every dummy image id and their layer ids that we found
found_any = False
for repo in repos:
    try:
        tags = get(f"/v2/{repo}/tags/list").get("tags") or []
    except Exception as exc:
        print(f"could not list tags for {repo}: {exc}", file=sys.stderr)
        sys.exit(1)
    for tag in tags:
        try:
            config_digest, chains = chains_of(repo, tag)
        except Exception as exc:
            print(f"could not resolve {repo}:{tag}: {exc}", file=sys.stderr)
            sys.exit(1)
        if not chains:
            continue
        print(f"CONFIG {config_digest}")
        for chain in chains:
            if chain not in own_infrastructure_chains:
                print(f"CHAIN {chain}")
        found_any = True

if not found_any:
    print("resolved zero corpus image manifests", file=sys.stderr)
    sys.exit(1)
PYEOF
)"
  CORPUS_EXPECTED_CONFIGS="$(printf '%s\n' "${raw}" | awk '$1=="CONFIG"{print $2}' | sort -u)"
  CORPUS_EXPECTED_CHAINS="$(printf '%s\n' "${raw}" | awk '$1=="CHAIN"{print $2}' | sort -u)"
}

## Helpers to see what images/containers/sandbox pods are on a specific cluster node
node_ctr_images() {
  docker exec "$1" ${CTR} images ls -q 2>&1
}

node_crictl_images() {
  docker exec "$1" crictl images 2>&1
}

node_ctr_snapshots() {
  docker exec "$1" ${CTR} snapshots ls 2>&1
}

node_crictl_containers() {
  docker exec "$1" crictl ps -a -o json 2>/dev/null
}

node_crictl_sandboxes() {
  docker exec "$1" crictl pods -o json 2>/dev/null
}

corpus_sandboxes_on() {
  local node="$1" containers_raw sandboxes_raw
  if ! containers_raw="$(node_crictl_containers "${node}")"; then
    return 1
  fi
  if ! sandboxes_raw="$(node_crictl_sandboxes "${node}")"; then
    return 1
  fi
  CONTAINERS_JSON="${containers_raw}" SANDBOXES_JSON="${sandboxes_raw}" \
    CORPUS_IMAGE_PREFIX="${CORPUS_PREFIX}" CORPUS_NAME_PREFIX="${CORPUS_POD_PREFIX}" \
    python3 - <<'PYEOF'
import json, os, sys

def parse(variable):
    raw = os.environ.get(variable, "").strip()
    if not raw:
        print(f"{variable} came back empty", file=sys.stderr)
        sys.exit(1)
    try:
        return json.loads(raw)
    except Exception as exc:
        print(f"could not read {variable}: {exc}", file=sys.stderr)
        sys.exit(1)

image_prefix = os.environ["CORPUS_IMAGE_PREFIX"]
name_prefix = os.environ["CORPUS_NAME_PREFIX"]

found = []
for container in parse("CONTAINERS_JSON").get("containers", []):
    reference = container.get("image", {}).get("userSpecifiedImage") or ""
    sandbox = container.get("podSandboxId")
    if sandbox and reference.startswith(image_prefix):
        found.append(sandbox)

for sandbox in parse("SANDBOXES_JSON").get("items", []):
    pod_name = sandbox.get("metadata", {}).get("name") or ""
    if sandbox.get("id") and pod_name.startswith(name_prefix):
        found.append(sandbox["id"])

seen = set()
for sandbox in found:
    if sandbox not in seen:
        seen.add(sandbox)
        print(sandbox)
PYEOF
}

# Gets all images from the container runtime that are running/installed by id rather than name to find all leftovers
corpus_ids_in() {
  local raw="$1" id expected
  printf '%s\n' "${raw}" | tail -n +2 | while read -r _ _ id _; do
    if [[ -z "${id}" ]]; then
      continue
    fi
    for expected in ${CORPUS_EXPECTED_CONFIGS}; do
      case "${expected#sha256:}" in
        "${id}"*)
          echo "${id}"
          break
          ;;
      esac
    done
  done | sort -u
}

# Gets all dummy images that have no name in the containerd image list
corpus_bare_ids_in() {
  local raw="$1" name expected
  printf '%s\n' "${raw}" | while read -r name; do
    case "${name}" in
      sha256:*) ;;
      *) continue ;;
    esac
    for expected in ${CORPUS_EXPECTED_CONFIGS}; do
      if [[ "${name}" == "${expected}" ]]; then
        echo "${name}"
        break
      fi
    done
  done | sort -u
}

# Unmount the nydus layers => deleting an image actually works afterwards
unmount_nydus_snapshots() {
  local node="$1"
  docker exec "${node}" sh -c '
    for m in /var/lib/rancher/k3s/agent/containerd-nydus/snapshots/*/mnt; do
      umount -f "${m}" 2>/dev/null || true
    done
  ' >/dev/null 2>&1 || true
}

# Cleanup all pods on a specific node that werent cleaned up automatically by kubernetes
force_stop_corpus_pods() {
  local node="$1" sandboxes pod_id
  if ! sandboxes="$(corpus_sandboxes_on "${node}")"; then
    return 0
  fi
  for pod_id in ${sandboxes}; do
    docker exec "${node}" crictl stopp "${pod_id}" >/dev/null 2>&1 || true
    docker exec "${node}" crictl rmp "${pod_id}" >/dev/null 2>&1 || true
  done
}

# Remove leftover layers from the dummy images starting from childs to parents
remove_corpus_snapshots() {
  local node="$1" chain made_progress sweep snapshots_raw present
  for sweep in $(seq 1 12); do
    if ! snapshots_raw="$(docker exec "${node}" ${CTR} snapshots ls 2>&1)"; then
      sleep 5
      continue
    fi
    present="$(comm -12 \
      <(printf '%s\n' "${CORPUS_EXPECTED_CHAINS}" | sort -u) \
      <(printf '%s\n' "${snapshots_raw}" | awk '{print $1}' | sort -u))"
    if [[ -z "${present}" ]]; then
      return 0
    fi
    made_progress=false
    for chain in ${present}; do
      if docker exec "${node}" ${CTR} snapshots rm "${chain}" >/dev/null 2>&1; then
        made_progress=true
      fi
    done
    if [[ "${made_progress}" == false ]]; then
      sleep 5
    fi
  done
}

# Remove every single dummy image including its layers from a specific node
purge_corpus_images() {
  local node="$1" name_refs id_refs ref
  local images_raw crictl_raw attempt
  force_stop_corpus_pods "${node}"
  unmount_nydus_snapshots "${node}"
  for attempt in $(seq 1 12); do
    name_refs=""
    id_refs=""
    if images_raw="$(node_ctr_images "${node}")"; then
      name_refs="$(printf '%s\n%s\n' \
        "$(printf '%s\n' "${images_raw}" | grep -F "${CORPUS_PREFIX}" || true)" \
        "$(corpus_bare_ids_in "${images_raw}")" | grep -v '^$' || true)"
    else
      name_refs="probe failed"
    fi
    
    if crictl_raw="$(node_crictl_images "${node}")"; then
      id_refs="$(corpus_ids_in "${crictl_raw}")"
    else
      id_refs="probe failed"
    fi
    
    if [[ -z "${name_refs}" && -z "${id_refs}" ]]; then
      remove_corpus_snapshots "${node}"
      return 0
    fi
    
    for ref in ${name_refs}; do
      docker exec "${node}" ${CTR} images rm --sync "${ref}" >/dev/null 2>&1 || true
    done
    
    for ref in ${id_refs}; do
      docker exec "${node}" crictl rmi "${ref}" >/dev/null 2>&1 || true
    done
    sleep 5
  done
  echo "${node}: could not purge ${CORPUS_PREFIX}* => delete manually!" >&2
  return 1
}

# Get the list of the namespace names of test pods in the cluster 
corpus_pods_now() {
  local raw
  if ! raw="$(kubectl get pods --all-namespaces --no-headers \
      -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name 2>/dev/null)"; then
    return 1
  fi
  printf '%s\n' "${raw}" | awk -v prefix="${CORPUS_POD_PREFIX}" \
    'index($2, prefix) == 1 { print $1, $2 }'
}

# Checks if every node is actually fully cold again
# This includes checking if every pod, image and its layers are fully removed
verify_cold() {
  local node problems images_raw crictl_raw snapshots_raw chain
  local corpus_pods namespace pod sandboxes sandbox
  problems=""

  if corpus_pods="$(corpus_pods_now)"; then
    while read -r namespace pod; do
      if [[ -n "${pod}" ]]; then
        problems="${problems}corpus pod ${namespace}/${pod} still exists"$'\n'
      fi
    done <<<"${corpus_pods}"
  else
    problems="${problems}could not verify that the corpus pods are fully terminated"$'\n'
  fi

  for node in "${NODES[@]}"; do
    if sandboxes="$(corpus_sandboxes_on "${node}")"; then
      for sandbox in ${sandboxes}; do
        problems="${problems}${node}: image container or pod sandbox ${sandbox} still present"$'\n'
      done
    else
      problems="${problems}${node}: could not verify containers and pod sandboxes to be fully removed"$'\n'
    fi

    if images_raw="$(node_ctr_images "${node}")"; then
      if printf '%s\n' "${images_raw}" | grep -qF "${CORPUS_PREFIX}"; then
        problems="${problems}${node}: some image names are still not cleaned"$'\n'
      fi
      
      if [[ -n "$(corpus_bare_ids_in "${images_raw}")" ]]; then
        problems="${problems}${node}: some images are still there without a name"$'\n'
      fi
    else
      problems="${problems}${node}: could not verify all image names are fully cleaned up"$'\n'
    fi

    if crictl_raw="$(node_crictl_images "${node}")"; then
      if [[ -n "$(corpus_ids_in "${crictl_raw}")" ]]; then
        problems="${problems}${node}: image ids still not fully cleaned up"$'\n'
      fi
    else
      problems="${problems}${node}: could not verify image ids are fully cleaned up"$'\n'
    fi

    if snapshots_raw="$(node_ctr_snapshots "${node}")"; then
      for chain in ${CORPUS_EXPECTED_CHAINS}; do
        if printf '%s\n' "${snapshots_raw}" | awk -v k="${chain}" '$1==k' | grep -q .; then
          problems="${problems}${node}: snapshot ${chain} still not cleaned up"$'\n'
        fi
      done
    else
      problems="${problems}${node}: could not verify snapshots are cleaned up"$'\n'
    fi
  done
  
  if [[ -n "${problems}" ]]; then
    if [[ "${1:-}" == "quiet" ]]; then
      return 1
    fi
    echo "Couldnt fully reset the cluster to cold state" >&2
    printf '%s' "${problems}" >&2
    exit 1
  fi
}

# Print all dummy services
corpus_services_now() {
  local raw
  if ! raw="$(kubectl get ksvc --all-namespaces --no-headers \
      -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name 2>/dev/null)"; then
    return 1
  fi
  {
    printf '%s\n' "${raw}" | awk -v prefix="${CORPUS_POD_PREFIX}" \
      'index($2, prefix) == 1 { print $1 "/" $2 }'
    echo "default/${SERVICE}"
  } | sort -u
}

# Delete a specific service
delete_service() {
  local namespace="$1" name="$2"
  for _ in $(seq 1 30); do
    if kubectl -n "${namespace}" delete ksvc "${name}" --ignore-not-found --timeout=120s; then
      return 0
    fi
    sleep 5
  done
  echo "service ${namespace}/${name} could not be deleted" >&2
  exit 1
}

# Delete all dummy/test services and any corresponding pods that are (potentially) stuck during termination
remove_corpus_workloads() {
  local services service namespace name owners remaining pod
  if ! services="$(corpus_services_now)"; then
    echo "couldnt ensure that no workloads are present anymore..." >&2
    exit 1
  fi

  for service in ${services}; do
    namespace="${service%%/*}"
    name="${service##*/}"
    delete_service "${namespace}" "${name}"

    kubectl -n "${namespace}" wait --for=delete replicaset,deploy \
      -l "serving.knative.dev/service=${name}" --timeout=60s >/dev/null 2>&1 || true
    if ! owners="$(kubectl -n "${namespace}" get replicaset,deploy \
        -l "serving.knative.dev/service=${name}" --no-headers 2>/dev/null)"; then
      echo "could not verify if ${service} has no deployment left" >&2
      exit 1
    fi
    if [[ -n "${owners}" ]]; then
      echo "${service} still has a deployment or replica set that would re-create its pods" >&2
      exit 1
    fi

    kubectl -n "${namespace}" wait --for=delete pod -l "serving.knative.dev/service=${name}" --timeout=15s >/dev/null 2>&1 || true
  done

  if ! remaining="$(corpus_pods_now)"; then
    echo "could not verify that the pods are gone" >&2
    exit 1
  fi
  if [[ -z "${remaining}" ]]; then
    return 0
  fi

  while read -r namespace pod; do
    if [[ -n "${pod}" ]]; then
      kubectl -n "${namespace}" delete pod "${pod}" --force --grace-period=0 --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
    fi
  done <<<"${remaining}"

  if ! remaining="$(corpus_pods_now)"; then
    echo "couldnt verify the pods are gone" >&2
    exit 1
  fi
  
  if [[ -n "${remaining}" ]]; then
    echo "final pass of trying to delete remaining pods failed" >&2
    printf '%s\n' "${remaining}" >&2
    exit 1
  fi
}

echo "Getting all dummy image data from the registry"
fetch_corpus_truth

echo "Removing the workloads before deleting the corresponding images"
remove_corpus_workloads

echo "Restarting cache-service to clear in-memory contents"
kubectl rollout restart deploy/nydus-cluster-cache
kubectl rollout status deploy/nydus-cluster-cache --timeout=120s

echo "Removing workload images and emptying local nydus cache"
for node in "${NODES[@]}"; do
  purge_corpus_images "${node}"
  "${REPO_ROOT}/scripts/node-snapshotter.sh" stop "${node}"
  docker exec "${node}" sh -c 'rm -rf /var/lib/rancher/k3s/agent/containerd-nydus/cache/* 2>/dev/null || true'
  if docker exec "${node}" sh -c "test -f ${DROPIN}"; then
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start "${node}"
  fi
done

echo "Restarting the chunk-proxy"
kubectl -n kube-system rollout restart ds/chunk-proxy
kubectl -n kube-system rollout status ds/chunk-proxy --timeout=180s

# Dragonfly's clients keep the pieces they downloaded on volumes that live
# only as long as the pod, so recreating them empties every peer. Recreating
# a node-network DaemonSet leaves the kubelet slow to start containers for a
# while, so the cluster is handed back only once a test pod starts on every
# node within four seconds, the same gate the managed-cloud reset uses
if [[ "${RESET_DRAGONFLY:-0}" == "1" ]]; then
  echo "Recreating the Dragonfly clients to empty their piece caches"
  kubectl -n dragonfly-system rollout restart ds/dragonfly-client
  kubectl -n dragonfly-system rollout status ds/dragonfly-client --timeout=300s
  for _ in $(seq 1 60); do
    "${REPO_ROOT}/scripts/dragonfly-rig.sh" healthy && break
    sleep 2
  done
  "${REPO_ROOT}/scripts/dragonfly-rig.sh" healthy || { echo "a Dragonfly client did not come back" >&2; exit 1; }

  for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    canary="settle-canary-${node##*-}"
    settled=""
    for _ in $(seq 1 10); do
      kubectl delete pod "${canary}" --ignore-not-found --timeout=60s >/dev/null 2>&1
      canary_start="$(date +%s.%N)"
      kubectl run "${canary}" --image=docker.io/library/busybox:latest \
        --image-pull-policy=IfNotPresent --restart=Never \
        --overrides="{\"spec\":{\"nodeName\":\"${node}\"}}" \
        --command -- sleep 3600 >/dev/null
      kubectl wait "pod/${canary}" --for=condition=Ready --timeout=120s >/dev/null
      canary_elapsed="$(awk -v a="$(date +%s.%N)" -v b="${canary_start}" 'BEGIN{printf "%.1f", a-b}')"
      kubectl delete pod "${canary}" --ignore-not-found --timeout=60s >/dev/null 2>&1
      if awk -v elapsed="${canary_elapsed}" 'BEGIN { exit !(elapsed < 4) }'; then
        settled="yes"
        break
      fi
      sleep 5
    done
    [[ -n "${settled}" ]] || echo "${node}: test pods still start slowly (${canary_elapsed}s), continuing" >&2
  done
fi

echo "Verifying every node actually is cold"
cold=""
for attempt in $(seq 1 12); do
  if verify_cold quiet; then
    cold="yes"
    break
  fi
  if [[ "${attempt}" = 1 ]]; then
    echo "waiting for the last sandbox pod to finish shutting down"
  fi
  sleep 5
done

[[ -n "${cold}" ]] || verify_cold
