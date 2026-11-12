#!/usr/bin/env bash
# Shared parsing for the slow-cache experiments, kept apart so it can be
# tested without a cluster

# Reads the delay in milliseconds out of a mode name such as slow25, and
# fails for any name that is not a slow mode
slow_mode_delay_ms() {
  local mode="$1"
  if [[ "${mode}" =~ ^slow([0-9]+)$ ]]; then
    echo "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

# Takes lines of "connect starttransfer" seconds as curl prints them and
# prints the median time the answer took after the connection stood, in
# milliseconds. That is the wait a proxy with an open connection sees
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

# Decides whether a measured added delay matches the one that was asked for,
# allowing a few milliseconds of jitter below and the pod's own noise above
delay_verified() {
  local wanted_ms="$1" added_ms="$2"
  awk -v wanted="${wanted_ms}" -v added="${added_ms}" \
    'BEGIN { exit !(added >= wanted - 5 && added <= wanted + 30) }'
}
