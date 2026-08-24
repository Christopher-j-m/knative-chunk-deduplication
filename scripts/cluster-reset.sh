#!/usr/bin/env bash
set -euo pipefail

# Returns the cluster to a true cold state between benchmark runs: removes the
# workload and purges every corpus image from every node, drops the in-memory
# cache contents, and clears the node-side nydus caches. Without the image
# purge a repeat run measures a warm start, because containerd still holds the
# image and its snapshots from the previous run. Safe to run in any
# configuration: the snapshotter is only restarted on nodes that carry the
# nydus drop-in (configs 2 and 4)
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mapfile -t NODES < <("${REPO_ROOT}/scripts/cluster-nodes.sh")
if [[ "${#NODES[@]}" -eq 0 ]]; then
  echo "no cluster nodes found; is the k3d cluster running?" >&2
  exit 1
fi
DROPIN=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.d/nydus-global.toml
CTR="ctr -a /run/k3s/containerd/containerd.sock -n k8s.io"
SERVICE="${SERVICE:-fn-python-hello}"

# Containerd keeps a bare image-id record (sha256:<config digest>) next to the
# tag and the digest names, so removing only the names that match the
# repository leaves the layers and snapshots referenced and the next "cold"
# pull finishes warm in a tenth of a second. Resolving each corpus image to
# its config digest and removing that too drops the whole chain. Every fn-
# image goes, not only the one under test: a chain left behind by another
# corpus image survives the blobcache wipe and the restarted snapshotter
# then revives its daemon over an empty cache
CORPUS_PREFIX="${CORPUS_PREFIX:-registry.localhost:5000/fn-}"

# Knative names every pod after the service it serves, so the same "fn-" that
# marks a corpus image also marks the pods and sandboxes that run one. That
# name is the only handle on a sandbox the kubelet has re-created but not yet
# filled with a container
CORPUS_POD_PREFIX="${CORPUS_PREFIX##*/}"
REGISTRY_CONTAINER=registry.localhost
REGISTRY_NETWORK=k3d-nydus-cache

# Filled in once per run by fetch_corpus_truth, before any node is touched
CORPUS_EXPECTED_CONFIGS=""
CORPUS_EXPECTED_CHAINS=""

# What counts as "corpus" has to come from the registry itself rather than
# from what a previous reset happened to notice, because a run that was
# interrupted partway through can leave a node's local image record already
# gone while the unpacked layers it produced are still sitting on disk. This
# recomputes, straight from the source images, every config digest and every
# depth of every layer chain a corpus image can produce, so a stale or
# missing local record can never hide a leftover snapshot from the checks
# below. A base layer that a corpus image happens to share with this
# script's own permanent infrastructure -- chunk-proxy and the cache
# service, both of which this script restarts on every run -- is excluded,
# because that layer is never actually gone from a real node either and
# chasing it would fail every single reset instead of only a dirty one
fetch_corpus_truth() {
  local registry_ip raw
  registry_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"${REGISTRY_NETWORK}\").IPAddress}}" "${REGISTRY_CONTAINER}")"
  raw="$(python3 - "${registry_ip}:5000" <<'PYEOF'
import sys, json, hashlib, urllib.request

registry = sys.argv[1]

def get(path, headers=None):
    request = urllib.request.Request(f"http://{registry}{path}", headers=headers or {})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read())

manifest_headers = {
    "Accept": ", ".join([
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    ])
}

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
    config_digest = manifest["config"]["digest"]
    config = get(f"/v2/{repo}/blobs/{config_digest}")
    diff_ids = config.get("rootfs", {}).get("diff_ids", [])
    if not diff_ids:
        return config_digest, []
    chains = [diff_ids[0]]
    for diff_id in diff_ids[1:]:
        chains.append("sha256:" + hashlib.sha256(f"{chains[-1]} {diff_id}".encode()).hexdigest())
    return config_digest, chains

try:
    catalog = get("/v2/_catalog")
except Exception as exc:
    print(f"error: could not reach registry at {registry}: {exc}", file=sys.stderr)
    sys.exit(1)

repos = [name for name in catalog.get("repositories", []) if name.startswith("fn-")]
if not repos:
    print("error: registry returned no fn- repositories; refusing to trust an empty corpus", file=sys.stderr)
    sys.exit(1)

own_infrastructure_chains = set()
for repo in ("chunk-proxy", "nydus-cluster-cache"):
    try:
        for tag in get(f"/v2/{repo}/tags/list").get("tags") or []:
            _, chains = chains_of(repo, tag)
            own_infrastructure_chains.update(chains)
    except Exception as exc:
        # A skipped image here is not a smaller list to check, it is a hole:
        # the gate would then trust a corpus chain that was actually this
        # script's own infrastructure, or miss excluding one that was. A
        # registry hiccup has to stop the whole reset, not narrow what it
        # verifies
        print(f"error: could not resolve {repo}'s own image chains: {exc}", file=sys.stderr)
        sys.exit(1)

found_any = False
for repo in repos:
    try:
        tags = get(f"/v2/{repo}/tags/list").get("tags") or []
    except Exception as exc:
        # Same reasoning as above, applied to the corpus images themselves:
        # a repository dropped here because of a transient error is a
        # repository the coldness gate will never check again this run
        print(f"error: could not list tags for {repo}: {exc}", file=sys.stderr)
        sys.exit(1)
    for tag in tags:
        try:
            config_digest, chains = chains_of(repo, tag)
        except Exception as exc:
            print(f"error: could not resolve {repo}:{tag}: {exc}", file=sys.stderr)
            sys.exit(1)
        if not chains:
            continue
        print(f"CONFIG {config_digest}")
        for chain in chains:
            if chain not in own_infrastructure_chains:
                print(f"CHAIN {chain}")
        found_any = True

if not found_any:
    print("error: resolved zero corpus image manifests; refusing to trust an empty corpus", file=sys.stderr)
    sys.exit(1)
PYEOF
)"
  CORPUS_EXPECTED_CONFIGS="$(printf '%s\n' "${raw}" | awk '$1=="CONFIG"{print $2}' | sort -u)"
  CORPUS_EXPECTED_CHAINS="$(printf '%s\n' "${raw}" | awk '$1=="CHAIN"{print $2}' | sort -u)"
}

# Raw image-name inventory for one node. Callers must check the exit status
# before trusting the text, because a broken exec channel prints nothing at
# all, and nothing at all must never be read the same way as a genuinely
# empty, clean node
node_ctr_images() {
  docker exec "$1" ${CTR} images ls -q 2>&1
}

# Raw crictl image inventory for one node, same rule: check the exit status
# first
node_crictl_images() {
  docker exec "$1" crictl images 2>&1
}

# Raw overlayfs snapshot inventory for one node, same rule again
node_ctr_snapshots() {
  docker exec "$1" ${CTR} snapshots ls 2>&1
}

# Raw container inventory for one node, same rule again. This one is read as
# structured data rather than as text, so a warning line on the side channel
# would corrupt it and is dropped; the exit status still says whether the
# listing happened at all
node_crictl_containers() {
  docker exec "$1" crictl ps -a -o json 2>/dev/null
}

# Raw pod-sandbox inventory for one node, same rule again
node_crictl_sandboxes() {
  docker exec "$1" crictl pods -o json 2>/dev/null
}

# Reports the pod sandbox ids on one node that belong to the corpus, either
# because a container inside one runs a corpus image or because the sandbox
# itself carries a corpus pod name. A sandbox that exists before its container
# does is precisely what the kubelet leaves behind while it re-creates a pod
# that was torn out from under it, so both listings are consulted. Returns
# non-zero when either listing cannot be read or cannot be understood
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
        print(f"error: {variable} came back empty", file=sys.stderr)
        sys.exit(1)
    try:
        return json.loads(raw)
    except Exception as exc:
        print(f"error: could not read {variable}: {exc}", file=sys.stderr)
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

# Picks the short image ids out of a raw crictl images listing that match one
# of the corpus images' real config digests fetched from the registry. This
# catches a dangling, untagged record exactly like a normally tagged one,
# because the id decides whether it belongs to the corpus, not whatever name
# containerd still happens to show next to it
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

# Picks the bare image-id records out of a raw containerd image listing. These
# rows carry no repository name at all, so the name check below cannot see
# them, and containerd's own listing is the only place they reliably show up:
# the runtime's separate view of the same images is rebuilt on restart and can
# lag behind it, which is how a purged image can reappear later
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

# A live nydus mount keeps the whole snapshot chain busy, so an image removal
# would leave the chain behind. Unmounting first lets the removal take it
unmount_nydus_snapshots() {
  local node="$1"
  docker exec "${node}" sh -c '
    for m in /var/lib/rancher/k3s/agent/containerd-nydus/snapshots/*/mnt; do
      umount -f "${m}" 2>/dev/null || true
    done
  ' >/dev/null 2>&1 || true
}

# Kubelet can take several minutes to notice a deleted pod and actually tear
# down its container -- Knative's own termination grace period runs to five
# minutes -- and until it does, the still-running container keeps the corpus
# image's snapshot chain pinned no matter how long the purge below is willing
# to wait. This stops and removes every pod sandbox that belongs to the
# corpus, the same way an operator would clear a stuck pod by hand, so that
# the gate at the end of the run finds none of them left
force_stop_corpus_pods() {
  local node="$1" sandboxes pod_id
  if ! sandboxes="$(corpus_sandboxes_on "${node}")"; then
    # A listing that cannot be read is not treated as an empty node here,
    # only as nothing to act on; the coldness gate at the end refuses to
    # pass a node whose listings it could not read
    return 0
  fi
  for pod_id in ${sandboxes}; do
    docker exec "${node}" crictl stopp "${pod_id}" >/dev/null 2>&1 || true
    docker exec "${node}" crictl rmp "${pod_id}" >/dev/null 2>&1 || true
  done
}

# Snapshots must be removed child before parent, but with every depth of
# every corpus image's chain already known up front there is no need to walk
# the tree by hand: repeatedly sweeping whatever the node actually has right
# now and removing whatever no longer has children reaches the same order on
# its own. The registry-wide chain list covers every corpus tag on every
# node, so most of it was never unpacked on this particular one -- checking
# what is really present before attempting a removal is what lets a clean
# node finish in one quick sweep instead of working through chains that were
# never here. A segment that still refuses removal after several sweeps with
# no further progress is presumed genuinely shared with something outside
# the corpus -- a live image or a container still mid-teardown -- and is
# left alone rather than forced
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

# Purges every corpus image and every snapshot it already produced from one
# node: stops any stuck pod still running a corpus image, removes the tag,
# digest, and bare id records until none are left, then removes the layer
# chains those images unpacked. A probe that fails to even run counts as
# "not clean yet" rather than "clean," so a broken exec channel or crictl
# hiccup extends the retry instead of ending it early
purge_corpus_images() {
  local node="$1" name_refs id_refs ref
  local images_raw crictl_raw attempt
  force_stop_corpus_pods "${node}"
  unmount_nydus_snapshots "${node}"
  for attempt in $(seq 1 12); do
    name_refs=""
    id_refs=""
    if images_raw="$(node_ctr_images "${node}")"; then
      # The bare id rows go on the same list because containerd removes them
      # by name like any other record, and leaving one behind is what lets a
      # purged image come back after the runtime rebuilds its own view
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
    # Everything containerd itself lists, named or not, is removed through ctr;
    # the ids that only the runtime's own listing knows about need its own
    # removal command instead
    for ref in ${name_refs}; do
      docker exec "${node}" ${CTR} images rm --sync "${ref}" >/dev/null 2>&1 || true
    done
    for ref in ${id_refs}; do
      docker exec "${node}" crictl rmi "${ref}" >/dev/null 2>&1 || true
    done
    sleep 5
  done
  echo "${node}: could not purge ${CORPUS_PREFIX}*; the next run would not be cold" >&2
  return 1
}

# Every corpus pod in the cluster, one "namespace name" row each. The cleanup
# before the purge and the gate at the end both read this same list, so the
# gate can never refuse to pass on a pod that nothing tried to clear
corpus_pods_now() {
  local raw
  if ! raw="$(kubectl get pods --all-namespaces --no-headers \
      -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name 2>/dev/null)"; then
    return 1
  fi
  printf '%s\n' "${raw}" | awk -v prefix="${CORPUS_POD_PREFIX}" \
    'index($2, prefix) == 1 { print $1, $2 }'
}

# The whole point of this script is that the next benchmark repetition can
# trust the word "cold," so it refuses to finish quietly unless it has
# positively observed that no corpus pod is left anywhere in the cluster and
# that no node still holds a corpus container, pod sandbox, image name, image
# id, or snapshot chain. The pod and sandbox checks matter as much as the
# image ones because the workload lands about a second after this returns:
# anything still able to pull would do so just past the gate. Every probe is
# checked against the registry-derived truth rather than against what this
# run's own purge happened to attempt, and a probe that fails to run counts
# as a failed verification, never as a clean result -- a silently warm
# configuration-1 repetition would understate eager pulling's cost and bias
# the comparison in the cache's own favor, which is worse than a loud,
# obvious failure here
verify_cold() {
  local node problems images_raw crictl_raw snapshots_raw chain
  local corpus_pods namespace pod sandboxes sandbox
  problems=""

  # A surviving pod object is the one leftover no node-side check can see in
  # time: the kubelet would re-create its sandbox and pull the image again
  # seconds after this gate returned, and the repetition that starts right
  # afterwards would measure a warm pull with nothing left to notice
  if corpus_pods="$(corpus_pods_now)"; then
    while read -r namespace pod; do
      if [[ -n "${pod}" ]]; then
        problems="${problems}corpus pod ${namespace}/${pod} still exists and would warm its node again
"
      fi
    done <<<"${corpus_pods}"
  else
    problems="${problems}could not verify that the corpus pods are gone, kubectl failed
"
  fi

  for node in "${NODES[@]}"; do
    if sandboxes="$(corpus_sandboxes_on "${node}")"; then
      for sandbox in ${sandboxes}; do
        problems="${problems}${node}: corpus container or pod sandbox ${sandbox} still present
"
      done
    else
      problems="${problems}${node}: could not verify containers and pod sandboxes, exec failed
"
    fi

    if images_raw="$(node_ctr_images "${node}")"; then
      if printf '%s\n' "${images_raw}" | grep -qF "${CORPUS_PREFIX}"; then
        problems="${problems}${node}: corpus image names still present
"
      fi
      if [[ -n "$(corpus_bare_ids_in "${images_raw}")" ]]; then
        problems="${problems}${node}: corpus image records still present without a name
"
      fi
    else
      problems="${problems}${node}: could not verify image names, exec failed
"
    fi

    if crictl_raw="$(node_crictl_images "${node}")"; then
      if [[ -n "$(corpus_ids_in "${crictl_raw}")" ]]; then
        problems="${problems}${node}: corpus image ids still present
"
      fi
    else
      problems="${problems}${node}: could not verify image ids, exec failed
"
    fi

    if snapshots_raw="$(node_ctr_snapshots "${node}")"; then
      for chain in ${CORPUS_EXPECTED_CHAINS}; do
        if printf '%s\n' "${snapshots_raw}" | awk -v k="${chain}" '$1==k' | grep -q .; then
          problems="${problems}${node}: snapshot ${chain} still present
"
        fi
      done
    else
      problems="${problems}${node}: could not verify snapshots, exec failed
"
    fi
  done
  if [[ -n "${problems}" ]]; then
    # the caller decides whether this is the final word: a probe run during
    # the grace period just reports back, only the last one gives up
    if [[ "${1:-}" == "quiet" ]]; then
      return 1
    fi
    echo "cluster-reset did not reach a genuinely cold state:" >&2
    printf '%s' "${problems}" >&2
    exit 1
  fi
}

# Names every corpus service that currently exists, plus the one under test
# even when it does not, as "namespace/name" rows. The service under test is
# not the only one that matters: switching configurations always leaves its
# own corpus service deployed, and a pod of that one would fail the gate at
# the end of a reset that never tried to remove it
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

# A configuration switch cycles the nodes right before a reset runs, and the
# Knative webhook rejects service writes until it serves again, so a single
# delete can fail on a webhook that is merely still starting. Retrying is what
# config-switch.sh already does for the same reason
delete_service() {
  local namespace="$1" name="$2"
  for _ in $(seq 1 30); do
    if kubectl -n "${namespace}" delete ksvc "${name}" --ignore-not-found --timeout=120s; then
      return 0
    fi
    sleep 5
  done
  echo "${namespace}/${name} could not be deleted; the Knative webhook never became reachable" >&2
  exit 1
}

# Removes every corpus service and waits until nothing is left that could put
# a pod back. The owning deployment and replica set go first and are confirmed
# gone, because those are what would create a replacement pod; only then are
# the pods themselves given a short moment to disappear on their own. They
# usually will not: Knative lets a revision's pods drain for as long as the
# revision's request timeout allows, five minutes by default, and waiting that
# out politely would cost more than everything else in this script put
# together. So whatever is still draining is removed outright, and a pod that
# survives even that stops the run rather than being left to warm a node later
remove_corpus_workloads() {
  local services service namespace name owners remaining pod
  if ! services="$(corpus_services_now)"; then
    echo "could not list the corpus services, so the reset cannot clear them" >&2
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
      echo "could not confirm ${service} has no deployment left" >&2
      exit 1
    fi
    if [[ -n "${owners}" ]]; then
      echo "${service} still has a deployment or replica set that would re-create its pods" >&2
      exit 1
    fi

    kubectl -n "${namespace}" wait --for=delete pod \
      -l "serving.knative.dev/service=${name}" --timeout=15s >/dev/null 2>&1 || true
  done

  if ! remaining="$(corpus_pods_now)"; then
    echo "could not confirm the corpus pods are gone, listing them failed" >&2
    exit 1
  fi
  if [[ -z "${remaining}" ]]; then
    return 0
  fi

  echo "corpus pods are still draining, removing them outright"
  while read -r namespace pod; do
    if [[ -n "${pod}" ]]; then
      kubectl -n "${namespace}" delete pod "${pod}" --force --grace-period=0 \
        --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
    fi
  done <<<"${remaining}"

  if ! remaining="$(corpus_pods_now)"; then
    echo "could not confirm the corpus pods are gone, listing them failed" >&2
    exit 1
  fi
  if [[ -n "${remaining}" ]]; then
    echo "corpus pods refuse to go away, so the next run would not be cold:" >&2
    printf '%s\n' "${remaining}" >&2
    exit 1
  fi
}

echo "Resolving the corpus straight from the registry"
fetch_corpus_truth

echo "Removing the workloads so their images can be purged from the nodes"
remove_corpus_workloads

echo "Restarting cache-service to clear in-memory contents"
kubectl rollout restart deploy/nydus-cluster-cache
kubectl rollout status deploy/nydus-cluster-cache --timeout=120s

echo "Purging workload images and clearing node nydus caches"
for node in "${NODES[@]}"; do
  purge_corpus_images "${node}"
  "${REPO_ROOT}/scripts/node-snapshotter.sh" stop "${node}"
  docker exec "${node}" sh -c 'rm -rf /var/lib/rancher/k3s/agent/containerd-nydus/cache/* 2>/dev/null || true'
  if docker exec "${node}" sh -c "test -f ${DROPIN}"; then
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start "${node}"
  fi
done

echo "Restarting the chunk-proxy DaemonSet"
kubectl -n kube-system rollout restart ds/chunk-proxy
kubectl -n kube-system rollout status ds/chunk-proxy --timeout=180s

echo "Verifying every node actually reached a cold state"
# Containerd tears sandboxes down asynchronously, so the last corpus sandbox
# can still be unwinding when the purge returns. A single look turns that lag
# into a failed benchmark cell even though the node reaches a cold state a few
# seconds later, so look again for up to a minute before believing it
cold=""
for attempt in $(seq 1 12); do
  if verify_cold quiet; then
    cold="yes"
    break
  fi
  if [[ "${attempt}" = 1 ]]; then
    echo "waiting for the last corpus sandbox to finish tearing down"
  fi
  sleep 5
done

# one final unquiet pass, which prints what is still wrong and exits
[[ -n "${cold}" ]] || verify_cold
echo "Reset complete"
