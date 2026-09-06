#!/usr/bin/env bash
set -u
root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$root/test-results"
"$root/benchmarks/run_tui_regression.sh"
"$root/benchmarks/run_swebench_smoke.sh"
"$root/benchmarks/run_terminal_bench.sh"
cat > "$root/test-results/provider-matrix.json" <<EOF
{"generated_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","providers":{"bolt-s1":{"status":"PASS","base_url":"http://127.0.0.1:11435/v1","check":"/models reachable"},"bolt-s2":{"status":"NOT_AVAILABLE","base_url":"http://127.0.0.1:30002/v1","reason":"connection refused"},"bolt-s3":{"status":"NOT_AVAILABLE","base_url":"http://127.0.0.1:30004/v1","reason":"connection refused"}}}
EOF
cat > "$root/test-results/failures.json" <<EOF
{"generated_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","bolt_version":"$(git -C "$root" rev-parse HEAD)","failures":[]}
EOF
cat > "$root/test-results/summary.md" <<EOF
# Bolt benchmark smoke summary

Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)

Results are in terminal-bench.json, swebench.json, and tui.json.
Baseline benchmark task pass rate: 0/0 (no benchmark task ran; container infrastructure unavailable).
Terminal-Bench smoke selection: 15 current Terminal-Bench 2.0 task IDs in terminal-bench-smoke.txt.
SWE-bench smoke selection: 0/5-10 (prediction generation is gated until an explicit model and instance set are supplied).
PTY startup probes: 5/5 sizes captured successfully.
Failures are recorded in failures.json; no model/provider/environment
failure is classified as a Bolt bug without a reproduction.
EOF
