#!/usr/bin/env bash
# Shared parsing helpers for the slow-cache experiments
slow_mode_delay_ms() {
  local mode="$1"
  if [[ "${mode}" =~ ^slow([0-9]+)$ ]]; then
    echo "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

median_answer_ms() {
  awk '
    NF >= 2 && $2 >= $1 { values[count++] = ($2 - $1) * 1000 }
    END {
      if (count == 0) { exit 1 }
      for (i = 0; i < count; i++) {
        for (j = i + 1; j < count; j++) {
          if (values[j] < values[i]) { swap = values[i]; values[i] = values[j]; values[j] = swap }
        }
      }
      if (count % 2 == 1) { median = values[(count - 1) / 2] }
      else { median = (values[count / 2 - 1] + values[count / 2]) / 2 }
      printf "%.1f\n", median
    }'
}

delay_verified() {
  local wanted_ms="$1" added_ms="$2"
  awk -v wanted="${wanted_ms}" -v added="${added_ms}" \
    'BEGIN { exit !(added >= wanted - 5 && added <= wanted + 30) }'
}
