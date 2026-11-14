#!/usr/bin/env bash
# Print the names of cluster node containers with the control node being the first one and then the workers (ordered by their number in name)
set -euo pipefail

CLUSTER="${K3D_CLUSTER:-nydus-cache}"
PREFIX="k3d-${CLUSTER}-"

# Get names by docker output of running containers
# Filter for the ones containing server or agent in name
# and then sort by number in their name, such as k3d-nydus-cache-agent-1
docker ps --format '{{.Names}}' \
  | grep -E "^${PREFIX}(server|agent)-[0-9]+$" \
  | awk -v prefix="${PREFIX}" '{
      suffix = substr($0, length(prefix) + 1)
      split(suffix, part, "-")
      rank = (part[1] == "server") ? 0 : 1
      printf "%d %010d %s\n", rank, part[2], $0
    }' \
  | sort -k1,1n -k2,2n \
  | awk '{print $3}'
