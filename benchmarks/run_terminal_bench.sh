#!/usr/bin/env bash
set -u
root=$(cd "$(dirname "$0")/.." && pwd)
report="$root/test-results/terminal-bench.json"
mkdir -p "$root/test-results"
if ! command -v uv >/dev/null 2>&1 || ! command -v docker >/dev/null 2>&1; then
  printf '{"benchmark":"terminal-bench","version":"2.0","status":"NOT_AVAILABLE","category":"D. benchmark/environment issue","reason":"uv and Docker/Podman are required"}\n' > "$report"
  echo "Terminal-Bench NOT_AVAILABLE: uv and Docker/Podman are required" >&2
  exit 0
fi
if [ ! -x "$root/bolt" ]; then GOCACHE=/tmp/agenterm-go-build make -C "$root" build; fi
export BOLT_BINARY="$root/bolt"
bolt_version=$(git -C "$root" rev-parse HEAD)
export BOLT_VERSION="$bolt_version"
export AGENTERM_BASE_URL="${AGENTERM_BASE_URL:-http://host.docker.internal:11435/v1}"
export AGENTERM_MODEL="${AGENTERM_MODEL:-qwen3-coder:latest}"
export AGENTERM_API_KEY="${AGENTERM_API_KEY:-ollama}"
args=()
while read -r task; do
  case "$task" in ''|\#*) ;; *) args+=(--include-task-name "$task") ;; esac
done < "$root/benchmarks/terminal-bench-smoke.txt"
if ! UV_CACHE_DIR=/tmp/agenterm-harbor-uv-cache UV_TOOL_DIR=/tmp/agenterm-uv-tools \
  uv tool run --from harbor harbor run --dataset terminal-bench@2.0 \
  --agent benchmarks.harbor.bolt_agent:Bolt --n-concurrent 1 --n-attempts 1 \
  --jobs-dir "$root/test-results/terminal-bench" "${args[@]}"; then
  printf '{"benchmark":"terminal-bench","version":"2.0","status":"NOT_AVAILABLE","category":"D. benchmark/environment issue","reason":"Harbor could not start the local container environment; inspect command output"}\n' > "$report"
  exit 0
fi
