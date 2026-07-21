#!/usr/bin/env bash
set -euo pipefail

# Emulates a remote registry by shaping the local registry container's
# egress with netem: every response leaving the registry pays the given
# delay and is capped to the given rate, so image pulls and on-demand
# chunk reads experience realistic wide-area latency while traffic that
# never touches the registry (such as cluster cache hits) stays fast.
#
# Usage: registry-netem.sh on [delay] [rate] | off | status
REGISTRY_CONTAINER="registry.localhost"
HELPER_IMAGE="nydus-netem-helper:local"
ACTION="${1:?usage: registry-netem.sh on [delay] [rate] | off | status}"
DELAY="${2:-50ms}"
RATE="${3:-200mbit}"

# The registry image ships no tc and no NET_ADMIN, so traffic shaping
# runs from a one-shot helper container that joins the registry's
# network namespace. Built locally once and reused afterwards
ensure_helper_image() {
  if ! docker image inspect "${HELPER_IMAGE}" >/dev/null 2>&1; then
    echo "Building ${HELPER_IMAGE}"
    docker build -q -t "${HELPER_IMAGE}" - <<'EOF' >/dev/null
FROM alpine:3.23
RUN apk add --no-cache iproute2-tc
ENTRYPOINT ["tc"]
EOF
  fi
}

run_tc() {
  docker run --rm "--net=container:${REGISTRY_CONTAINER}" --cap-add=NET_ADMIN \
    "${HELPER_IMAGE}" "$@"
}

# Docker hands out eth0/eth1 in attach order, and that order can change when
# the container restarts, so the device carrying the cluster-network address
# has to be looked up instead of assumed. Shaping the wrong device silently
# leaves cluster traffic at full speed
cluster_device() {
  local cluster_ip
  cluster_ip="$(docker inspect \
    -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' \
    "${REGISTRY_CONTAINER}")"
  docker run --rm "--net=container:${REGISTRY_CONTAINER}" --entrypoint ip \
    "${HELPER_IMAGE}" -o -4 addr show \
    | awk -v wanted="${cluster_ip}/" 'index($4, wanted) == 1 { print $2; exit }'
}

ensure_helper_image
DEVICE="$(cluster_device)"
if [[ -z "${DEVICE}" ]]; then
  echo "could not find the registry device on the cluster network" >&2
  exit 1
fi

case "${ACTION}" in
  on)
    run_tc qdisc replace dev "${DEVICE}" root netem delay "${DELAY}" rate "${RATE}"
    echo "Registry egress shaped on ${DEVICE}: delay ${DELAY}, rate ${RATE}"
    ;;
  off)
    run_tc qdisc del dev "${DEVICE}" root 2>/dev/null || true
    echo "Registry egress shaping removed"
    ;;
  status)
    run_tc qdisc show dev "${DEVICE}"
    ;;
  *)
    echo "unknown action ${ACTION}; choose on, off, or status" >&2
    exit 1
    ;;
esac
