#!/usr/bin/env bash
# Simulate distance to the registry by adding delay and/or bandwidth cap to every output of the registry
# Example: registry-netem.sh on 80ms 1gbit => delays all output by 80ms and caps bandwidth at max. 1 gbit/s
# LIMIT=<packets> sets how many packets netem may hold at once; tc's default is 1000
set -euo pipefail

REGISTRY_CONTAINER="registry.localhost"
HELPER_IMAGE="nydus-netem-helper:local"
ACTION="${1:?usage: registry-netem.sh on [delay] [rate] | off | status}"
DELAY="${2:-50ms}"
RATE="${3:-200mbit}"
LIMIT="${LIMIT:-}"

# Build a image that only contains tc, since the registry container doesnt has it yet
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

# Run a tc command inside the registry network via the tc image
run_tc() {
  docker run --rm "--net=container:${REGISTRY_CONTAINER}" --cap-add=NET_ADMIN "${HELPER_IMAGE}" "$@"
}

# Gets the network card that is used by the registry for output to the cluster
# => THis apparently changes after restarts
cluster_device() {
  local cluster_ip
  cluster_ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' "${REGISTRY_CONTAINER}")"
  docker run --rm "--net=container:${REGISTRY_CONTAINER}" --entrypoint ip "${HELPER_IMAGE}" -o -4 addr show | awk -v wanted="${cluster_ip}/" 'index($4, wanted) == 1 { print $2; exit }'
}


ensure_helper_image
DEVICE="$(cluster_device)"
if [[ -z "${DEVICE}" ]]; then
  echo "wasnt able not find the registry in the cluster network..." >&2
  exit 1
fi

case "${ACTION}" in
  on)
    # Every packet the registry sends waits in netem's queue for the whole delay,
    # so the queue length caps how many packets all clients together can have in
    # flight. Left at tc's default of 1000 unless the caller sets a longer one
    limit_option=()
    if [[ -n "${LIMIT}" ]]; then
      limit_option=(limit "${LIMIT}")
    fi

    # Create a qdisc traffic rule that delays everything the registry sends and throttle the bandwidth as well based on input
    run_tc qdisc replace dev "${DEVICE}" root netem delay "${DELAY}" rate "${RATE}" "${limit_option[@]}"
    echo "Registry output capped on ${DEVICE}: delay ${DELAY}, bandwidth rate ${RATE}${LIMIT:+, queue ${LIMIT} packets}"
    ;;
  off)
    # Delete the qdisc traffic rule again that throttled the reigstry output
    run_tc qdisc del dev "${DEVICE}" root 2>/dev/null || true
    echo "Removed the capping of the registry output"
    ;;
  status)
    # The statistics include how many packets netem dropped because its queue
    # was full; they accumulate until the rule is removed, so compare two readings
    run_tc -s qdisc show dev "${DEVICE}"
    ;;
  *)
    echo "unknown tsc action ${ACTION}" >&2
    exit 1
    ;;
esac
