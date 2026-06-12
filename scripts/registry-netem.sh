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

ensure_helper_image

case "${ACTION}" in
  on)
    run_tc qdisc replace dev eth0 root netem delay "${DELAY}" rate "${RATE}"
    echo "Registry egress shaped: delay ${DELAY}, rate ${RATE}"
    ;;
  off)
    run_tc qdisc del dev eth0 root 2>/dev/null || true
    echo "Registry egress shaping removed"
    ;;
  status)
    run_tc qdisc show dev eth0
    ;;
  *)
    echo "unknown action ${ACTION}; choose on, off, or status" >&2
    exit 1
    ;;
esac
