#!/usr/bin/env bash
# Checks for the AKS experiments that an eager pull really moved the image across the network, sourced by the experiment driver
REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
# shellcheck source=cold-guard.sh
source "${REPO_ROOT}/scripts/experiments/cold-guard.sh"

PROBE_NAMESPACE="coldprobe"

guard_nodeexec() {
  kubectl -n kube-system exec "$1" -- nsenter -t 1 -m -p -n -- sh -c "$2"
}

guard_setup_pods() {
  kubectl -n kube-system get pods -l app=nydus-node-setup -o jsonpath='{.items[*].metadata.name}'
}

compressed_layer_bytes() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]]["oci"]["transfer_bytes"])' \
    "${IMAGE_FACTS:?set IMAGE_FACTS}" "$1"
}

corpus_fingerprints() {
  python3 - "${IMAGE_FACTS:?set IMAGE_FACTS}" "$1" <<'PYEOF'
import json
import sys

facts, kind = json.load(open(sys.argv[1])), sys.argv[2]
values = set()
for image in facts.values():
    for tag in image.values():
        values.update(tag[kind])
print("\n".join(sorted(values)))
PYEOF
}

assert_nodes_hold_no_corpus() {
  local acr="${ACR:?set ACR}" digests chains pod findings report=""
  digests="$(corpus_fingerprints layer_digests)"
  chains="$(corpus_fingerprints chain_ids)"

  for pod in $(guard_setup_pods); do
    findings="$(guard_nodeexec "${pod}" "
      crictl images 2>/dev/null | awk -v corpus='${acr}/fn-' 'index(\$1, corpus) == 1 { print \"image \" \$1 \":\" \$2 }'
      for namespace in k8s.io ${PROBE_NAMESPACE}; do
        ctr -n \"\${namespace}\" content ls -q 2>/dev/null | sed \"s|^|content \${namespace} |\"
        for snapshotter in overlayfs nydus; do
          ctr -n \"\${namespace}\" snapshots --snapshotter \"\${snapshotter}\" ls 2>/dev/null \
            | awk -v where=\"\${namespace}/\${snapshotter}\" 'NR > 1 { print \"snapshot \" where \" \" \$1 }'
        done
      done
    " 2>/dev/null || echo "unreachable")"

    if [[ "${findings}" == "unreachable" ]]; then
      report+="  ${pod}: node could not be inspected"$'\n'
      continue
    fi

    report+="$(awk -v pod="${pod}" '
      FILENAME == ARGV[1] { wanted[$1] = 1; next }
      $1 == "image" { print "  " pod ": " $0; next }
      $1 == "content" && ($3 in wanted) { print "  " pod ": " $0; next }
      $1 == "snapshot" && ($3 in wanted) { print "  " pod ": " $0 }
    ' <(printf '%s\n%s\n' "${digests}" "${chains}") - <<<"${findings}")"$'\n'
  done

  if [[ -n "${report//[$'\n ']/}" ]]; then
    echo "cold guard: nodes still hold corpus content before the seed:" >&2
    echo "${report}" >&2
    return 1
  fi
  echo "cold guard: no node holds any corpus image, layer, or snapshot"
}

measure_pull_rate() {
  local image="$1" acr="${ACR:?set ACR}" pod bytes seconds
  pod="$(guard_setup_pods | awk '{ print $1 }')"
  bytes="$(compressed_layer_bytes "${image}")"

  seconds="$(guard_nodeexec "${pod}" "
    wipe() {
      ctr -n ${PROBE_NAMESPACE} images rm --sync ${acr}/${image}:oci >/dev/null 2>&1 || true
      ctr -n ${PROBE_NAMESPACE} content ls -q 2>/dev/null | xargs -r ctr -n ${PROBE_NAMESPACE} content rm >/dev/null 2>&1 || true
    }
    wipe
    ctr -n ${PROBE_NAMESPACE} content fetch --platform linux/amd64 ${acr}/${image}:oci >/dev/null 2>&1 || exit 1
    wipe
    start=\$(date +%s.%N)
    ctr -n ${PROBE_NAMESPACE} content fetch --platform linux/amd64 ${acr}/${image}:oci >/dev/null 2>&1 || exit 1
    end=\$(date +%s.%N)
    wipe
    awk -v a=\"\${end}\" -v b=\"\${start}\" 'BEGIN { printf \"%.3f\", a - b }'
  ")" || { echo "cold guard: reference fetch of ${image} failed" >&2; return 1; }

  awk -v b="${bytes}" -v s="${seconds}" 'BEGIN { printf "%.0f %s %s\n", b / s, b, s }'
}

assert_cell_received_bytes() {
  local csv="$1" image="$2" bytes violations
  bytes="$(compressed_layer_bytes "${image}")"
  violations="$(awk -F, -v need="${bytes}" \
    'NR > 1 && $5 == "no" && ($10 == "" || $10 + 0 < need) { print "  rep " $2 " " $3 " " $4 " node_rx_bytes " ($10 == "" ? "missing" : $10) }' \
    "${csv}")"
  if [[ -n "${violations}" ]]; then
    echo "cold guard: cold rows received fewer than the ${bytes} compressed bytes of ${image}:" >&2
    echo "${violations}" >&2
    mv "${csv}" "${csv%.csv}.failed-cold"
    echo "cold guard: kept the evidence at ${csv%.csv}.failed-cold" >&2
    return 1
  fi
}
