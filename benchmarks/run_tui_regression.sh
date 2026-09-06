#!/usr/bin/env bash
set -u
root=$(cd "$(dirname "$0")/.." && pwd)
report="$root/test-results/tui.json"
frames="$root/test-results/tui-frames"
mkdir -p "$frames"
if ! command -v script >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
  printf '{"benchmark":"bolt-pty","status":"NOT_AVAILABLE","category":"D. benchmark/environment issue","reason":"script and timeout are required"}\n' > "$report"
  exit 0
fi
GOCACHE=/tmp/agenterm-go-build make -C "$root" build >/dev/null
ok=1
for size in 80x24 100x30 120x40 160x40 200x50; do
  cols=${size%x*}; rows=${size#*x}
  COLUMNS="$cols" LINES="$rows" timeout 20 script -qefc \
    "$root/bolt --no-mcp --ping" "$frames/$size.typescript" >/dev/null 2>&1 || ok=0
  [ -s "$frames/$size.typescript" ] || ok=0
done
if [ "$ok" -eq 1 ]; then
  printf '{"benchmark":"bolt-pty","status":"PASS","sizes":["80x24","100x30","120x40","160x40","200x50"],"structural_checks":["startup command completed","one captured PTY frame per size"]}\n' > "$report"
else
  printf '{"benchmark":"bolt-pty","status":"FAIL","category":"C. provider failure or D. benchmark/environment issue","reason":"one or more deterministic startup probes failed; inspect captured PTY frames"}\n' > "$report"
fi
