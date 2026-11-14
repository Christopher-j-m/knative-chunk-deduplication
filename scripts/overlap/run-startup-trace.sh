#!/usr/bin/env bash
# Records which chunks each image reads at startup by mounting its Nydus bootstrap and walking its startup files
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

REGISTRY="${REGISTRY:-localhost:5000}"
NYDUSD="${NYDUSD:-${HOME}/.local/bin/nydusd}"
WORKDIR="${WORKDIR:-/tmp/chunk-inspector}"
OUT_DIR="${OUT_DIR:-/tmp/startup-trace}"
CACHE_DIR="${CACHE_DIR:-/tmp/nydus-cache}"
MOUNT_BASE="${MOUNT_BASE:-/tmp/nydus-mount}"
NYDUS_TAG="${NYDUS_TAG:-nydus}"
SETTLE_SECS="${SETTLE_SECS:-2}"
CHUNKS_JSON="${CHUNKS_JSON:-${PROJECT_DIR}/analysis/results/chunks-startup-base.json}"
CHUNK_INSPECTOR="${CHUNK_INSPECTOR:-${PROJECT_DIR}/bin/chunk-inspector}"

IMAGES=(
  fn-python-hello
  fn-python-ml
  fn-python-api
  fn-node-hello
  fn-node-api
  fn-go-hello
  fn-rust-hello
  fn-java-hello
)

declare -A STARTUP_PATHS
STARTUP_PATHS[fn-python-hello]='/usr/local/bin/python3.14 /usr/local/lib/python3.14 /app'
STARTUP_PATHS[fn-python-ml]='/usr/local/bin/python3.14 /usr/local/lib/python3.14 /app'
STARTUP_PATHS[fn-python-api]='/usr/local/bin/python3.14 /usr/local/lib/python3.14 /app'
STARTUP_PATHS[fn-node-hello]='/usr/local/bin/node /usr/local/lib/node_modules /app'
STARTUP_PATHS[fn-node-api]='/usr/local/bin/node /usr/local/lib/node_modules /app'
STARTUP_PATHS[fn-go-hello]='/handler'
STARTUP_PATHS[fn-rust-hello]='/handler'
STARTUP_PATHS[fn-java-hello]='/opt/java /app /__cacert_entrypoint.sh'

if ! "$NYDUSD" --version >/dev/null 2>&1; then
  echo "ERROR: nydusd not runnable at $NYDUSD" >&2
  exit 1
fi

if ! grep -qE '^[[:space:]]*user_allow_other' /etc/fuse.conf 2>/dev/null; then
  echo "ERROR: /etc/fuse.conf does not contain 'user_allow_other'." >&2
  echo "nydusd v2.4 hardcodes 'allow_other' in the FUSE mount opts." >&2
  echo "Run as root: echo user_allow_other | sudo tee -a /etc/fuse.conf" >&2
  exit 1
fi

if [[ ! -x "$CHUNK_INSPECTOR" ]]; then
  echo "ERROR: $CHUNK_INSPECTOR not found or not executable." >&2
  echo "Run before: go build -o ${PROJECT_DIR}/bin/chunk-inspector ./cmd/chunk-inspector" >&2
  exit 1
fi

mkdir -p "$OUT_DIR" "$CACHE_DIR"

if [[ ! -f "$CHUNKS_JSON" ]]; then
  echo "Generating chunks mapping at $CHUNKS_JSON ..."
  mkdir -p "$(dirname "$CHUNKS_JSON")"
  "$CHUNK_INSPECTOR" --output "$CHUNKS_JSON" --insecure "${IMAGES[@]}"
fi

sanitize_ref() {
  printf '%s' "$1" | sed 's#[/:]#_#g'
}

paths_to_hashes() {
  local image="$1"
  local mount="$2"
  local path_list="$3"
  local out_hashes="$4"

  python3 - "$image" "$mount" "$path_list" "$out_hashes" "$CHUNKS_JSON" <<'PY'
import json, os, sys

image_id, mount, path_list, out_hashes, chunks_json = sys.argv[1:6]

with open(chunks_json) as fh:
    data = json.load(fh)

img = next((entry for entry in data if entry.get("image") == image_id), None)
if img is None:
    sys.exit(f"image {image_id!r} not in {chunks_json}")

path_to_hashes = {}
for c in img.get("chunks", []):
    fp = c.get("file_path") or ""
    if not fp:
        continue
    path_to_hashes.setdefault(fp, []).append(c["hash"])

hashes = set()
matched_paths = 0
unmatched_paths = 0
with open(path_list) as fh:
    for line in fh:
        p = line.rstrip("\n")
        if not p:
            continue
        if not p.startswith(mount):
            unmatched_paths += 1
            continue
        rel = p[len(mount):]
        if not rel.startswith("/"):
            rel = "/" + rel
        hh = path_to_hashes.get(rel)
        if hh:
            for h in hh:
                hashes.add(h)
            matched_paths += 1
        else:
            unmatched_paths += 1

with open(out_hashes, "w") as fh:
    for h in sorted(hashes):
        fh.write(h + "\n")

print(f"  paths matched={matched_paths} unmatched={unmatched_paths} unique_hashes={len(hashes)}")
PY
}

trace_cleanup() {
  local pid="$1"
  local mount="$2"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    local i
    for i in 1 2 3 4 5; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
    wait "$pid" 2>/dev/null || true
  fi
  if [[ -n "$mount" ]]; then
    fusermount3 -u "$mount" >/dev/null 2>&1 || true
  fi
}

trace_one() {
  local image="$1"
  local ref="${REGISTRY}/${image}:${NYDUS_TAG}"
  local sanitized
  sanitized="$(sanitize_ref "$ref")"
  local bootstrap="${WORKDIR}/${sanitized}/image.boot"
  local mount="${MOUNT_BASE}-${image}"
  local logfile="${OUT_DIR}/nydusd-${image}.log"
  local config="${OUT_DIR}/nydusd-${image}.json"
  local pathfile="${OUT_DIR}/paths-${image}.txt"
  local hashfile="${OUT_DIR}/accessed-${image}.txt"
  local pid=""

  trap 'trace_cleanup "$pid" "$mount"' RETURN

  echo "${image}"

  if [[ ! -f "$bootstrap" ]]; then
    echo "SKIP: bootstrap not found at $bootstrap" >&2
    echo "Run chunk-inspector to repopulate bootstraps." >&2
    return 1
  fi

  mkdir -p "$mount" "${CACHE_DIR}/${image}"

  cat > "$config" <<EOF
{
  "device": {
    "backend": {
      "type": "registry",
      "config": {
        "scheme": "http",
        "host": "${REGISTRY}",
        "repo": "${image}",
        "timeout": 5,
        "connect_timeout": 5,
        "retry_limit": 2
      }
    },
    "cache": {
      "type": "blobcache",
      "config": {
        "work_dir": "${CACHE_DIR}/${image}"
      }
    }
  },
  "mode": "direct",
  "digest_validate": false,
  "iostats_files": false,
  "enable_xattr": true,
  "fs_prefetch": {
    "enable": false,
    "threads_count": 1,
    "merging_size": 1048576,
    "prefetch_all": false
  }
}
EOF

  fusermount3 -u "$mount" >/dev/null 2>&1 || true
  rm -f "$logfile" "${logfile}.stdout" "$hashfile" "$pathfile"

  "$NYDUSD" fuse \
    --config "$config" \
    --bootstrap "$bootstrap" \
    --mountpoint "$mount" \
    --rlimit-nofile 524288 \
    --log-level info \
    --log-file "$logfile" \
    >"${logfile}.stdout" 2>&1 &
  pid=$!

  sleep "$SETTLE_SECS"

  if ! kill -0 "$pid" 2>/dev/null; then
    echo "  ERROR: nydusd exited before mount" >&2
    tail -n 20 "${logfile}.stdout" 2>/dev/null | sed 's/^/    /' >&2 || true
    tail -n 20 "$logfile" 2>/dev/null | sed 's/^/    /' >&2 || true
    return 1
  fi

  : > "$pathfile"
  if [[ -z "${STARTUP_PATHS[$image]+x}" ]]; then
    echo "ERROR: no STARTUP_PATHS entry for image '$image'." >&2
    echo "Add the cold-start path set for '$image' to STARTUP_PATHS in $(basename "$0")." >&2
    return 1
  fi
  local paths="${STARTUP_PATHS[$image]}"
  local present=0 missing=0
  for p in $paths; do
    local target="${mount}${p}"
    if [[ -d "$target" ]]; then
      find "$target" -type f -print >> "$pathfile" 2>/dev/null || true
      find "$target" -type f -print0 2>/dev/null | xargs -0 -r cat >/dev/null 2>&1 || true
      present=$((present + 1))
    elif [[ -f "$target" ]]; then
      printf '%s\n' "$target" >> "$pathfile"
      cat "$target" >/dev/null 2>&1 || true
      present=$((present + 1))
    else
      echo "note: path missing under mount: $p" >&2
      missing=$((missing + 1))
    fi
  done
  echo "startup paths: $present present, $missing missing; $(wc -l < "$pathfile") files enumerated"
  
  trace_cleanup "$pid" "$mount"
  pid=""

  paths_to_hashes "$image" "$mount" "$pathfile" "$hashfile"
  echo "wrote $hashfile ($(wc -l < "$hashfile") hashes)"
}

failures=0
for image in "${IMAGES[@]}"; do
  if ! trace_one "$image"; then
    failures=$((failures + 1))
  fi
done

if (( failures > 0 )); then
  echo "" >&2
  echo "$failures/${#IMAGES[@]} traces failed." >&2
  exit 1
fi

echo ""
echo "Done => Input the results into chunk-inspector with:"
echo "${CHUNK_INSPECTOR} --output analysis/results/chunks-startup.json --accessed-log-dir $OUT_DIR --insecure ${IMAGES[*]}"
