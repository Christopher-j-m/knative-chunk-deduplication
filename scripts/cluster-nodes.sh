#!/usr/bin/env bash
set -euo pipefail

# Prints the container names of the benchmark cluster's Kubernetes nodes, the
# server first and the agents in index order. Every script that reaches into
# the nodes reads the list from here, so growing the cluster for a node-count
# sweep does not mean editing the same array in four places. The load
# balancer, the registry and any other cluster's containers are excluded.
CLUSTER="${K3D_CLUSTER:-nydus-cache}"
PREFIX="k3d-${CLUSTER}-"

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
