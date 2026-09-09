# Bolt agent evaluation

## Standalone Bolt Coding Eval

This Docker-free harness creates isolated temporary Go repositories, invokes
the existing headless Bolt binary, and independently checks every task:

```bash
python3 benchmarks/bolt_coding_eval.py --self-test
python3 benchmarks/bolt_coding_eval.py \
  --base-url "$AGENTERM_BASE_URL" \
  --model "/sglang-data/models/gpt-oss-120b" \
  --api-key "$AGENTERM_API_KEY"
```

Results are written to `test-results/bolt-coding-eval.json` and
`test-results/bolt-coding-eval.md`; per-task logs and saved Bolt state are
under `test-results/bolt-coding-eval/<task-id>/`. Use the same task set and
only change `--model`/provider settings for future model comparisons.

This directory is the reproducible evaluation entry point. Benchmark source
checkouts belong outside this repository and are never modified.

## Current upstreams (researched 2026-09-06)

- Terminal-Bench 2.0 (`terminal-bench@2.0`) is the current benchmark. Harbor is
  its official harness; the legacy `tb` harness is not used here. Harbor 0.22.0
  was installed in `/tmp/agenterm-uv-tools` for this run.
- Harbor adapters use `BaseInstalledAgent`, implementing `install()` and
  `run(instruction, environment, context)`. This repository provides
  `benchmarks.harbor.bolt_agent:Bolt`.
- Terminal-Bench tasks contain `instruction.md`, `task.toml`, an environment,
  solution, and verifier. Harbor supports `--include-task-name` glob filters,
  `--n-tasks`, `--n-concurrent`, and `--jobs-dir`.
- SWE-bench's current v5 CLI uses the `verified` and `lite` aliases and the
  existing Docker evaluator (`swebench eval verified ...`). Verified is the
  500-task human-validated subset; Lite is the smaller published subset.

## Terminal-Bench smoke run

Build first, then run sequentially. `BOLT_BINARY` is uploaded into each task
container, so Harbor invokes the real Bolt binary rather than the provider API.

```bash
GOCACHE=/tmp/agenterm-go-build make build
export BOLT_BINARY="$PWD/bolt"
export BOLT_VERSION="$(git rev-parse HEAD)"
export AGENTERM_BASE_URL="http://host.docker.internal:11435/v1"
export AGENTERM_MODEL="qwen3-coder:latest"
export AGENTERM_API_KEY="ollama"
UV_CACHE_DIR=/tmp/agenterm-harbor-uv-cache \
UV_TOOL_DIR=/tmp/agenterm-uv-tools \
uv tool run --from harbor harbor run \
  --dataset terminal-bench@2.0 \
  --agent benchmarks.harbor.bolt_agent:Bolt \
  --n-concurrent 1 --n-attempts 1 \
  --jobs-dir test-results/terminal-bench \
  --include-task-name fix-git --include-task-name fix-code-vulnerability
```

Use `--n-concurrent 2` only after the sequential baseline is complete. A
Docker/Podman daemon, task image pulls, and a reachable provider are required.
Unavailable infrastructure is recorded as `NOT_AVAILABLE`, not as a Bolt
failure. The complete smoke list is in `terminal-bench-smoke.txt`.

## SWE-bench smoke run

Generate predictions with a real Bolt-driven runner, then use the official
evaluator. Do not replace the evaluator with local assertions:

```bash
./benchmarks/run_swebench_smoke.sh
swebench eval verified -p test-results/swebench/predictions.json \
  --run-id bolt-smoke-$(date -u +%Y%m%dT%H%M%SZ) -j 1
```

The script is intentionally gated on the required `swebench`, Docker, model,
and repository inputs; it writes a structured `NOT_AVAILABLE` record when a
dependency is absent.

## Reports and TUI suite

Each runner writes JSON under `test-results/` with task, model, provider, Bolt
revision, workspace, timeout, tool transcript/log paths, verifier result, and
failure category. `run_tui_regression.sh` drives the built binary through a
PTY at fixed sizes and records structural frame checks; package-level Bubble
Tea tests remain the deterministic fast suite.
