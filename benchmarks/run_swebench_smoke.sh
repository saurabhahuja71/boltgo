#!/usr/bin/env bash
set -u
root=$(cd "$(dirname "$0")/.." && pwd)
report="$root/test-results/swebench.json"
mkdir -p "$root/test-results/swebench"
if ! command -v swebench >/dev/null 2>&1 || ! command -v docker >/dev/null 2>&1; then
  printf '{"benchmark":"swe-bench","dataset":"verified","status":"NOT_AVAILABLE","category":"D. benchmark/environment issue","reason":"swebench CLI and Docker/Podman are required"}\n' > "$report"
  echo "SWE-bench NOT_AVAILABLE: swebench CLI and Docker/Podman are required" >&2
  exit 0
fi
printf '{"benchmark":"swe-bench","dataset":"verified","status":"NOT_AVAILABLE","category":"D. benchmark/environment issue","reason":"Prediction-generation adapter requires an explicit instance selection and model endpoint","bolt_version":"%s"}\n' "$(git -C "$root" rev-parse HEAD)" > "$report"
echo "SWE-bench prediction generation is gated; no fabricated predictions were produced." >&2
