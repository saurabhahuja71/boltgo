#!/usr/bin/env bash
set -u

# Uses the caller's already-exported key but never prints or saves it.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bolt="${BOLT_BINARY:-$root/bolt}"
base_url="${AGENTERM_BASE_URL:-https://openrouter.ai/api/v1}"
api_key="${AGENTERM_API_KEY:-${OPENROUTER_API_KEY:-}}"
timeout_seconds="${FREE_MODEL_TIMEOUT_SECONDS:-240}"
results_dir="${FREE_MODEL_RESULTS_DIR:-$root/test-results/free-models}"

if [[ -z "$api_key" ]]; then
  echo 'No API key available. Run: source ~/.bashrc && openrouter' >&2
  exit 2
fi
[[ -x "$bolt" ]] || { echo "Bolt binary not found: $bolt" >&2; exit 2; }
mkdir -p "$results_dir"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/bolt-free-models.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

# Discover current free models. Authorization is used only in this subprocess.
if ! curl --fail --silent --show-error --connect-timeout 15 --max-time 60 \
  -H "Authorization: Bearer $api_key" \
  "$base_url/models" -o "$tmp/models.json"; then
  echo "Could not reach $base_url/models. Check the proxy/network path and retry." >&2
  exit 1
fi
[[ -s "$tmp/models.json" ]] || { echo "OpenRouter returned an empty model catalog." >&2; exit 1; }
python3 - "$tmp/models.json" "$tmp/models.tsv" <<'PY'
import json, sys
data = json.load(open(sys.argv[1], encoding="utf-8"))
with open(sys.argv[2], "w", encoding="utf-8") as out:
    for m in sorted(data.get("data", []), key=lambda x: x.get("id", "")):
        p = m.get("pricing") or {}
        if str(p.get("prompt")) == "0" and str(p.get("completion")) == "0":
            mid = m.get("id", "")
            if mid:
                print(mid, m.get("name", "").replace("\t", " "), sep="\t", file=out)
PY

[[ -s "$tmp/models.tsv" ]] || { echo "OpenRouter returned no free models." >&2; exit 1; }

summary="$results_dir/summary.tsv"
printf 'model\tgo_pass\tpython_pass\tgo_seconds\tpython_seconds\tstatus\n' > "$summary"

run_python_task() {
  local model="$1" workspace="$2" log="$3"
  mkdir -p "$workspace"
  cat > "$workspace/slugger.py" <<'PY'
def slugify(value: str) -> str:
    return value
PY
  cat > "$workspace/test_slugger.py" <<'PY'
from slugger import slugify
def test_contract():
    assert slugify("  Hello, Python World!  ") == "hello-python-world"
    assert slugify("A---B___C") == "a-b-c"
    assert slugify("   ") == ""
PY
  local prompt='Implement slugify in slugger.py. Trim whitespace, lowercase ASCII letters, replace each run of non-alphanumeric characters with one hyphen, trim hyphens, and return empty for blank input. Run pytest -q before finishing.'
  local start end rc test_rc
  start="$(date +%s.%N)"
  timeout "$timeout_seconds" "$bolt" --no-resume --no-mcp --workspace "$workspace" \
    --base-url "$base_url" --model "$model" --api-key "$api_key" exec "$prompt" >"$log" 2>&1
  rc=$?
  end="$(date +%s.%N)"
  pytest -q "$workspace" >/dev/null 2>&1
  test_rc=$?
  python3 - "$start" "$end" "$rc" "$test_rc" <<'PY'
from decimal import Decimal
import sys
elapsed = Decimal(sys.argv[2]) - Decimal(sys.argv[1])
print(f"{int(sys.argv[3] == '0' and sys.argv[4] == '0')}\t{elapsed:.3f}")
PY
}

while IFS=$'\t' read -r model _name; do
  safe="${model//[^a-zA-Z0-9_.-]/_}"
  dir="$tmp/$safe"
  mkdir -p "$dir"
  go_log="$dir/go.log"
  go_start="$(date +%s.%N)"
  if timeout "$timeout_seconds" python3 "$root/benchmarks/bolt_coding_eval.py" \
      --bolt "$bolt" --base-url "$base_url" --model "$model" --api-key "$api_key" \
      --provider openrouter --timeout "$timeout_seconds" --task 04-slugify-feature \
      >"$go_log" 2>&1; then
    go_pass="$(python3 -c 'import json; print(int(json.load(open("'"$root"'/test-results/bolt-coding-eval.json"))["tasks"][0]["success"]))' 2>/dev/null || echo 0)"
  else
    go_pass=0
  fi
  go_end="$(date +%s.%N)"
  go_seconds="$(python3 - "$go_start" "$go_end" <<'PY'
from decimal import Decimal
import sys
print(f"{Decimal(sys.argv[2]) - Decimal(sys.argv[1]):.3f}")
PY
)"
  py_result="$(run_python_task "$model" "$dir/python" "$dir/python.log")"
  python_pass="${py_result%%$'\t'*}"
  python_seconds="${py_result#*$'\t'}"
  if [[ "$go_pass" == 1 && "$python_pass" == 1 ]]; then status=PASS; else status=FAIL_OR_RATE_LIMIT; fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$model" "$go_pass" "$python_pass" "$go_seconds" "$python_seconds" "$status" >> "$summary"
  echo "$model: Go=$go_pass Python=$python_pass Go=${go_seconds}s Python=${python_seconds}s"
done < "$tmp/models.tsv"

python3 - "$summary" "$results_dir/report.md" <<'PY'
import csv, sys
rows = list(csv.DictReader(open(sys.argv[1]), delimiter="\t"))
for r in rows:
    r["quality"] = int(r["go_pass"]) + int(r["python_pass"])
    r["speed"] = float(r["go_seconds"]) + float(r["python_seconds"])
quality = sorted(rows, key=lambda r: (-r["quality"], r["speed"]))[:3]
speed = sorted(rows, key=lambda r: (r["speed"], -r["quality"]))[:3]
lines = ["# Bolt OpenRouter free-model benchmark", "", f"Models attempted: {len(rows)}", "", "## Top 3 reasoning/quality", "", "Model | Passed tasks | Total seconds", "---|---:|---:"]
lines += [f"{r['model']} | {r['quality']}/2 | {r['speed']:.1f}" for r in quality]
lines += ["", "## Top 3 speed", "", "Model | Total seconds | Passed tasks", "---|---:|---:"]
lines += [f"{r['model']} | {r['speed']:.1f} | {r['quality']}/2" for r in speed]
lines += ["", "## All results", "", "Model | Go | Python | Go seconds | Python seconds | Status", "---|---:|---:|---:|---:|---"]
lines += [f"{r['model']} | {r['go_pass']} | {r['python_pass']} | {float(r['go_seconds']):.1f} | {float(r['python_seconds']):.1f} | {r['status']}" for r in rows]
open(sys.argv[2], "w").write("\n".join(lines) + "\n")
print("\n".join(lines))
PY
echo "Full report: $results_dir/report.md"
