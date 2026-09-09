#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
report="$root/test-results/terminal-bench-smoke.json"
mkdir -p "$root/test-results"
if ! command -v uv >/dev/null 2>&1 || ! command -v docker >/dev/null 2>&1 || ! command -v ssh >/dev/null 2>&1; then
  printf '{"benchmark":"terminal-bench","version":"2.0","status":"NOT_AVAILABLE","category":"FAIL-ENVIRONMENT","reason":"uv, Docker/Podman, and ssh are required"}\n' > "$report"
  echo "Terminal-Bench NOT_AVAILABLE: uv, Docker/Podman, and ssh are required" >&2
  exit 0
fi
if [ ! -x "$root/bolt" ]; then GOCACHE=/tmp/agenterm-go-build make -C "$root" build; fi
export BOLT_BINARY="$root/bolt"
bolt_version=$(git -C "$root" rev-parse HEAD)
export BOLT_VERSION="$bolt_version"
export AGENTERM_BASE_URL="${AGENTERM_BASE_URL:-http://host.containers.internal:23005/v1}"
export AGENTERM_MODEL="${AGENTERM_MODEL:-/sglang-data/models/gpt-oss-120b}"
export AGENTERM_API_KEY="${AGENTERM_API_KEY:-sglang}"

# Keep the existing dev-vm/OCI S3 provider. This extra forward is ephemeral
# and binds only for the smoke run so rootless Harbor containers can reach it
# through host.containers.internal; no persistent tunnel/config is changed.
remote_host="${BOLT_DEV_VM_HOST:-100.94.149.55}"
remote_port="${BOLT_S3_REMOTE_PORT:-30004}"
host_port="${BOLT_S3_CONTAINER_PORT:-23005}"
ssh_key="${BOLT_DEV_VM_KEY:-$HOME/.ssh/id_rsa}"
known_hosts="${BOLT_DEV_VM_KNOWN_HOSTS:-/tmp/dev-vm-known-hosts}"
ssh_opts=(-F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no
  -o UserKnownHostsFile="$known_hosts" -o ServerAliveInterval=30
  -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -i "$ssh_key")
ssh "${ssh_opts[@]}" "sauahuja@$remote_host" "bash -ic 'source ~/.bashrc >/dev/null 2>&1; sglang3-up'"
ssh "${ssh_opts[@]}" -g -N -L "0.0.0.0:${host_port}:127.0.0.1:${remote_port}" "sauahuja@$remote_host" \
  >/tmp/bolt-terminal-bench-s3-tunnel.log 2>&1 &
tunnel_pid=$!
trap 'kill "$tunnel_pid" 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  if curl --noproxy '*' -fsS --max-time 2 "http://host.containers.internal:${host_port}/v1/models" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
if ! docker run --rm docker.io/library/redis:7-alpine sh -c \
  "env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY wget -qO- --timeout=8 http://host.containers.internal:${host_port}/v1/models" >/dev/null; then
  printf '{"benchmark":"terminal-bench","version":"2.0","status":"NOT_AVAILABLE","category":"FAIL-ENVIRONMENT","reason":"S3 endpoint is not reachable from a task container"}\n' > "$report"
  echo "Terminal-Bench NOT_AVAILABLE: S3 endpoint is not reachable from a task container" >&2
  exit 0
fi
args=()
while read -r task; do
  case "$task" in ''|\#*) ;; *) args+=(--include-task-name "$task") ;; esac
done < "$root/benchmarks/terminal-bench-smoke.txt"
if ! PATH="$root/benchmarks:$PATH" \
  PYTHONPATH="$root${PYTHONPATH:+:$PYTHONPATH}" \
  UV_CACHE_DIR=/tmp/agenterm-harbor-uv-cache UV_TOOL_DIR=/tmp/agenterm-uv-tools \
  uv tool run --from harbor harbor run --dataset terminal-bench@2.0 \
  --agent benchmarks.harbor.bolt_agent:Bolt --n-concurrent 1 --n-attempts 1 \
  --allow-agent-host host.containers.internal \
  --jobs-dir "$root/test-results/terminal-bench-smoke" "${args[@]}"; then
  printf '{"benchmark":"terminal-bench","version":"2.0","status":"NOT_AVAILABLE","category":"FAIL-ENVIRONMENT","reason":"Harbor could not start the local container environment; inspect job logs"}\n' > "$report"
  exit 0
fi

result_path=$(find "$root/test-results/terminal-bench-smoke" -mindepth 2 -maxdepth 2 \
  -type f -name result.json | sort | tail -n 1)
python3 - "$result_path" "$report" "$AGENTERM_MODEL" "$root/test-results/terminal-bench-smoke" <<'PY'
import json
import pathlib
import sys

result_path, report_path, model, jobs_dir = sys.argv[1:]
result = json.loads(pathlib.Path(result_path).read_text())
stats = result.get("stats", {})
total = stats.get("n_total_trials", 0)
errors = stats.get("n_errored_trials", 0)
completed = stats.get("n_completed_trials", 0)
if errors == total and total:
    status = "FAIL-ENVIRONMENT"
elif errors:
    status = "COMPLETED_WITH_ERRORS"
else:
    status = "COMPLETED"
report = {
    "benchmark": "terminal-bench",
    "version": "2.0",
    "status": status,
    "provider": "sglang",
    "model": model,
    "jobs_dir": jobs_dir,
    "n_total_trials": total,
    "n_completed_trials": completed,
    "n_errored_trials": errors,
    "result": str(pathlib.Path(result_path)),
}
pathlib.Path(report_path).write_text(json.dumps(report, indent=2) + "\n")
PY
