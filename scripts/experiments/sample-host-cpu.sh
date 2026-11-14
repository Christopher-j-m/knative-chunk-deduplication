#!/usr/bin/env bash
# Records the host and node container processor load once per second until stopped
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUTFILE="${1:?usage: sample-host-cpu.sh <outfile>}"

mapfile -t NODES < <("${REPO_ROOT}/scripts/cluster/cluster-nodes.sh")
declare -A CGROUP
for node in "${NODES[@]}"; do
  id="$(docker inspect -f '{{.Id}}' "${node}")"
  CGROUP["${node}"]="/sys/fs/cgroup/system.slice/docker-${id}.scope/cpu.stat"
done

host_jiffies() {
  awk '$1 == "cpu" { total = 0; for (i = 2; i <= NF; i++) total += $i
    print total, $5, $6 }' /proc/stat
}

pressure_usec() {
  awk '$1 == "some" { split($5, field, "="); print field[2] }' /proc/pressure/cpu
}

container_usec() {
  awk '$1 == "usage_usec" { print $2 }' "$1" 2>/dev/null || echo 0
}

header="timestamp,host_idle_pct,host_iowait_pct,cpu_pressure_pct"
for node in "${NODES[@]}"; do
  header="${header},${node#k3d-nydus-cache-}_cpus"
done
echo "${header}" >"${OUTFILE}"

read -r total_before idle_before iowait_before < <(host_jiffies)
pressure_before="$(pressure_usec)"
declare -A USAGE_BEFORE
for node in "${NODES[@]}"; do
  USAGE_BEFORE["${node}"]="$(container_usec "${CGROUP[${node}]}")"
done
clock_before="$(date +%s%N)"

while true; do
  sleep 1
  read -r total idle iowait < <(host_jiffies)
  pressure="$(pressure_usec)"
  clock="$(date +%s%N)"
  elapsed_usec=$(( (clock - clock_before) / 1000 ))

  line="$(date -u +%FT%T.%3NZ)"
  line="${line},$(awk -v t=$((total - total_before)) -v i=$((idle - idle_before)) 'BEGIN { printf "%.1f", 100 * i / t }')"
  line="${line},$(awk -v t=$((total - total_before)) -v w=$((iowait - iowait_before)) 'BEGIN { printf "%.1f", 100 * w / t }')"
  line="${line},$(awk -v p=$((pressure - pressure_before)) -v e="${elapsed_usec}" 'BEGIN { printf "%.1f", 100 * p / e }')"
  for node in "${NODES[@]}"; do
    usage="$(container_usec "${CGROUP[${node}]}")"
    line="${line},$(awk -v u=$((usage - USAGE_BEFORE[${node}])) -v e="${elapsed_usec}" 'BEGIN { printf "%.2f", u / e }')"
    USAGE_BEFORE["${node}"]="${usage}"
  done
  echo "${line}" >>"${OUTFILE}"

  total_before="${total}"
  idle_before="${idle}"
  iowait_before="${iowait}"
  pressure_before="${pressure}"
  clock_before="${clock}"
done
