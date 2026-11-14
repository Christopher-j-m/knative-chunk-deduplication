#!/usr/bin/env bash
# Slows the cluster cache down by delaying everything the cache pod sends
set -euo pipefail

HELPER_IMAGE="nydus-netem-helper:local"
CACHE_LABEL="${CACHE_LABEL:-app=nydus-cluster-cache}"
ACTION="${1:?usage: cache-netem.sh on <delay> | off | status}"
LIMIT="${CACHE_NETEM_LIMIT:-100000}"
DELAY="${2:-}"

ensure_helper_image() {
  if ! docker image inspect "${HELPER_IMAGE}" >/dev/null 2>&1; then
    echo "cache-netem: ${HELPER_IMAGE} is missing; run registry-netem.sh status once to build it" >&2
    exit 1
  fi
}

cache_pod_location() {
  local listing pods
  listing="$(kubectl get pods -l "${CACHE_LABEL}" \
    -o jsonpath='{range .items[*]}{.status.phase}|{.metadata.deletionTimestamp}|{.spec.nodeName}|{.status.containerStatuses[0].containerID}{"\n"}{end}')"
  pods="$(awk -F'|' '$1 == "Running" && $2 == "" { print $3 " " $4 }' <<<"${listing}")"
  if [[ "$(grep -c . <<<"${pods}")" != "1" ]] || [[ "$(grep -c . <<<"${listing}")" != "1" ]]; then
    echo "cache-netem: expected exactly one running cache pod, found:" >&2
    echo "${listing}" >&2
    exit 1
  fi
  echo "${pods}"
}

location="$(cache_pod_location)"
node="${location%% *}"
container_id="${location##*://}"
ensure_helper_image
pod_pid="$(docker exec "${node}" crictl inspect "${container_id}" \
  | python3 -c 'import json, sys; print(json.load(sys.stdin)["info"]["pid"])')"
if [[ -z "${pod_pid}" || "${pod_pid}" == "0" ]]; then
  echo "cache-netem: could not find the process of the cache pod on ${node}" >&2
  exit 1
fi

run_tc_in_pod() {
  docker run --rm --privileged "--pid=container:${node}" --entrypoint nsenter \
    "${HELPER_IMAGE}" -t "${pod_pid}" -n tc "$@"
}

case "${ACTION}" in
  on)
    if [[ -z "${DELAY}" ]]; then
      echo "usage: cache-netem.sh on <delay>" >&2
      exit 1
    fi
    run_tc_in_pod qdisc replace dev eth0 root netem delay "${DELAY}" limit "${LIMIT}"
    echo "Cache pod on ${node} answers ${DELAY} late, queue ${LIMIT} packets"
    ;;
  off)
    dropped="$(run_tc_in_pod -s qdisc show dev eth0 2>/dev/null \
      | awk '/netem/ { shaped = 1 } shaped && /dropped/ { gsub(",", ""); for (i = 1; i <= NF; i++) if ($i == "(dropped") print $(i + 1) }' | head -1)"
    [[ -n "${dropped}" ]] && echo "Cache delay dropped ${dropped} packets"
    run_tc_in_pod qdisc del dev eth0 root 2>/dev/null || true
    echo "Removed the delay from the cache pod on ${node}"
    ;;
  status)
    run_tc_in_pod -s qdisc show dev eth0
    ;;
  *)
    echo "unknown action ${ACTION}" >&2
    exit 1
    ;;
esac
