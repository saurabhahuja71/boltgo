#!/usr/bin/env python3
"""Standalone, Docker-free Bolt coding evaluation harness.

The harness is deliberately not an agent layer.  It creates a fixture, invokes
the existing Bolt headless command once, and independently checks the result.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
RESULTS = ROOT / "test-results" / "bolt-coding-eval"


BASE = {
    "go.mod": "module example.com/evalfixture\n\ngo 1.25\n",
    "calc/calc.go": '''package calc

// Add returns the sum of two integers.
func Add(a, b int) int { return a + b }

// Slugify is intentionally incomplete for the evaluation task.
func Slugify(s string) string { return s }

// Average returns the integer average.
func Average(a, b int) int { return (a + b) / 2 }
''',
    "calc/calc_test.go": '''package calc

import "testing"

func TestAdd(t *testing.T) {
    if Add(2, 3) != 5 { t.Fatal("Add") }
}

func TestAverage(t *testing.T) {
    if Average(4, 6) != 5 { t.Fatal("Average") }
}
''',
    "cmd/app/main.go": '''package main

import (
    "fmt"
    "os"
    "example.com/evalfixture/calc"
)

func main() {
    if len(os.Args) > 1 { fmt.Println(calc.Slugify(os.Args[1])) }
}
''',
    "README.md": "# Evaluation fixture\n\nThe calc package provides integer helpers.\n",
}


@dataclass(frozen=True)
class Task:
    id: str
    category: str
    prompt: str
    check: str
    expected: dict


def task_set() -> list[Task]:
    return [
        Task("01-locate-behavior", "repository-understanding", "Inspect the repository and write REPORT.md explaining which package implements Add, citing the exact file path and function name. Do not modify Go source.", "report", {"terms": ["calc/calc.go", "func Add"]}),
        Task("02-trace-function", "repository-understanding", "Trace the Slugify call from the CLI through the repository. Write TRACE.md naming both source files and the function call path. Do not modify Go source.", "trace", {"terms": ["cmd/app/main.go", "calc/calc.go", "Slugify"]}),
        Task("03-fix-average", "simple-implementation", "Fix Average so it handles negative inputs correctly without integer overflow for normal int values. Add or update focused tests and run go test ./... .", "average", {}),
        Task("04-slugify-feature", "simple-implementation", "Implement Slugify: trim surrounding whitespace, lowercase ASCII letters, replace each run of non-alphanumeric characters with one hyphen, and trim hyphens. Add focused tests and run go test ./... .", "slugify", {}),
        Task("05-empty-edge", "simple-implementation", "Make Slugify return an empty string for empty or whitespace-only input and add a regression test for that edge case. Run go test ./... .", "empty", {}),
        Task("06-multifile-normalize", "multifile-implementation", "Add calc.NormalizePair in calc/normalize.go that returns the two integers in ascending order. Export it through a new cmd/app helper function NormalizeArgs in cmd/app/main.go. Add tests and run go test ./... .", "normalize", {}),
        Task("07-dependent-api", "multifile-implementation", "Change Average to accept a slice of ints and update every caller and test. Preserve the empty-slice behavior as zero. Run go test ./... .", "average-slice", {}),
        Task("08-add-unit-test", "testing", "Add a focused unit test for Add with two negative numbers in calc/calc_test.go. Run go test ./... .", "negative-test", {}),
        Task("09-regression-test", "testing", "Add a regression test proving Slugify collapses repeated punctuation into one hyphen. Do not claim completion unless the test is actually present, and run go test ./... .", "slugify-test", {}),
        Task("10-update-tests", "testing", "Update the Average tests to cover an odd total and document the expected integer truncation. Run go test ./... .", "average-test", {}),
        Task("11-failing-test-recovery", "debugging-recovery", "First inspect the failing behavior in Average, then fix it so Average(-4, -6) returns -5. Run the relevant test and then go test ./... .", "negative-average", {}),
        Task("12-diagnose-cli", "debugging-recovery", "Diagnose why the CLI currently prints raw input punctuation. Fix the implementation so `go run ./cmd/app ' Hello, World! '` prints `hello-world`, add a test, and verify it.", "cli", {}),
        Task("13-wrong-assumption", "debugging-recovery", "Add a ParseAndSlug helper in calc/parse.go that accepts a string and returns Slugify(s). Before editing, inspect the existing package: there is no parser package. Put the helper in calc, add a test, and run go test ./... .", "wrong-assumption", {}),
        Task("14-safe-refactor", "refactoring", "Refactor Add to use a private add helper while keeping the exported Add API and behavior unchanged. Add a test that proves the public API still works and run go test ./... .", "refactor", {}),
        Task("15-internal-interface", "refactoring", "Introduce a private Formatter interface in calc and make Slugify use a small implementation of it. Preserve all existing behavior, add a focused test, and run go test ./... .", "interface", {}),
        Task("16-multiple-criteria", "acceptance-criteria", "Complete every requirement: (1) implement Slugify as specified, (2) add regression tests for spaces and punctuation, (3) update README.md with one usage example, and (4) run go test ./... .", "multi", {}),
        Task("17-implementation-tests-verify", "acceptance-criteria", "Complete all three independent criteria: implement NormalizePair in a new file, add unit tests for both orderings, and run go test ./... successfully. A passing test alone does not satisfy the implementation or test-file criteria.", "normalize-tests", {}),
        Task("18-independent-criteria", "acceptance-criteria", "Make all independent changes: add a Version() string function returning `1.0`, add version_test.go testing it, update README.md to mention Version, and run go test ./... . Do not infer any one criterion from another.", "version", {}),
        Task("19-explore-first", "agent-behavior", "Explore the repository with read/search tools before modifying it. Then add calc.Clamp(n, low, high), test below-range/in-range/above-range cases, and run go test ./... .", "clamp", {}),
        Task("20-final-verification", "agent-behavior", "Perform this multi-step task: implement calc.SafeAverage for a non-empty slice, add tests for normal and empty input, update README.md, then run go test ./... and report the verification result. Do not finish before the final verification.", "safe-average", {}),
    ]


def write_fixture(workspace: Path) -> None:
    for name, content in BASE.items():
        path = workspace / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
    subprocess.run(["git", "init", "-q"], cwd=workspace, check=True)
    subprocess.run(["git", "config", "user.email", "eval@example.invalid"], cwd=workspace, check=True)
    subprocess.run(["git", "config", "user.name", "Bolt Eval"], cwd=workspace, check=True)
    subprocess.run(["git", "add", "."], cwd=workspace, check=True)
    subprocess.run(["git", "commit", "-qm", "fixture baseline"], cwd=workspace, check=True)


def run_checked(cmd: list[str], cwd: Path, timeout: int = 120, env: dict[str, str] | None = None) -> tuple[int, str, float]:
    started = time.monotonic()
    try:
        p = subprocess.run(cmd, cwd=cwd, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
        return p.returncode, p.stdout, time.monotonic() - started
    except subprocess.TimeoutExpired as exc:
        return 124, (exc.stdout or "") + "\nTIMEOUT\n", time.monotonic() - started


def external_check(task: Task, workspace: Path) -> tuple[bool, str, float]:
    if task.check == "report":
        p = workspace / "REPORT.md"
        return p.exists() and all(x.lower() in p.read_text().lower() for x in task.expected["terms"]), "report terms", 0.0
    if task.check == "trace":
        p = workspace / "TRACE.md"
        return p.exists() and all(x.lower() in p.read_text().lower() for x in task.expected["terms"]), "trace terms", 0.0
    check_env = os.environ.copy()
    check_env["GOCACHE"] = "/tmp/bolt-coding-eval-gocache"
    check_env["GOPATH"] = "/tmp/bolt-coding-eval-gopath"
    check_env["GOMODCACHE"] = "/tmp/bolt-coding-eval-gopath/pkg/mod"
    code, out, elapsed = run_checked(["go", "test", "./..."], workspace, env=check_env)
    if code != 0:
        return False, "go test failed:\n" + out[-4000:], elapsed
    checks = {
        "average": lambda: "func Average(a, b int) int" in (workspace / "calc/calc.go").read_text(),
        "slugify": lambda: "strings.ToLower" in (workspace / "calc/calc.go").read_text() and "Slugify" in (workspace / "calc/calc.go").read_text(),
        "empty": lambda: "Slugify" in (workspace / "calc/calc_test.go").read_text() and "empty" in (workspace / "calc/calc_test.go").read_text().lower(),
        "normalize": lambda: (workspace / "calc/normalize.go").exists() and "NormalizePair" in (workspace / "cmd/app/main.go").read_text(),
        "average-slice": lambda: "[]int" in (workspace / "calc/calc.go").read_text(),
        "negative-test": lambda: "-2" in (workspace / "calc/calc_test.go").read_text(),
        "slugify-test": lambda: "Slugify" in (workspace / "calc/calc_test.go").read_text() and ("!!!" in (workspace / "calc/calc_test.go").read_text() or "punctuation" in (workspace / "calc/calc_test.go").read_text().lower()),
        "average-test": lambda: "trunc" in (workspace / "calc/calc_test.go").read_text().lower() or "odd" in (workspace / "calc/calc_test.go").read_text().lower(),
        "negative-average": lambda: "Average(-4, -6)" in (workspace / "calc/calc_test.go").read_text() or "-5" in (workspace / "calc/calc_test.go").read_text(),
        "cli": lambda: "strings" in (workspace / "calc/calc.go").read_text(),
        "wrong-assumption": lambda: (workspace / "calc/parse.go").exists() and "ParseAndSlug" in (workspace / "calc/parse.go").read_text(),
        "refactor": lambda: "func add(" in (workspace / "calc/calc.go").read_text(),
        "interface": lambda: "type Formatter interface" in (workspace / "calc/calc.go").read_text(),
        "multi": lambda: "usage" in (workspace / "README.md").read_text().lower() and len(list(workspace.glob("**/*_test.go"))) >= 2,
        "normalize-tests": lambda: (workspace / "calc/normalize.go").exists() and "NormalizePair" in "".join(p.read_text() for p in workspace.glob("**/*_test.go")),
        "version": lambda: (workspace / "calc/version_test.go").exists() and "Version" in (workspace / "README.md").read_text(),
        "clamp": lambda: "Clamp" in (workspace / "calc/calc.go").read_text() and "Clamp" in "".join(p.read_text() for p in workspace.glob("**/*_test.go")),
        "safe-average": lambda: "SafeAverage" in (workspace / "calc/calc.go").read_text() and "README" in "".join(p.read_text() for p in [workspace / "README.md"]),
    }
    ok = checks.get(task.check, lambda: True)()
    return ok, "external go test + task assertions", elapsed


_RUN_STATE_FIELDS = {
    "original_goal": "OriginalGoal",
    "acceptance_criteria": "AcceptanceCriteria",
    "completed_criteria": "CompletedCriteria",
    "acceptance_criteria_state": "AcceptanceCriteriaState",
    "plan": "Plan",
    "current_step": "CurrentStep",
    "phase": "Phase",
    "tool_calls": "ToolCalls",
    "observations": "Observations",
    "failures": "Failures",
    "verification_criteria": "VerificationCriteria",
    "verification": "Verification",
    "iterations": "Iterations",
    "retries": "Retries",
    "tool_calls_used": "ToolCallsUsed",
}


def _state_value(raw: dict, lower_name: str):
    """Read a state field from either the current Go or legacy JSON spelling."""
    if lower_name in raw:
        return raw[lower_name]
    return raw.get(_RUN_STATE_FIELDS[lower_name])


def _normalize_state_items(items):
    if not isinstance(items, list):
        return items
    normalized = []
    for item in items:
        if not isinstance(item, dict):
            normalized.append(item)
            continue
        entry = dict(item)
        for lower_name, go_name in {
            "tool": "Tool",
            "summary": "Summary",
            "success": "Success",
            "satisfied": "Satisfied",
            "status": "Status",
        }.items():
            if lower_name not in entry and go_name in entry:
                entry[lower_name] = entry[go_name]
        normalized.append(entry)
    return normalized


def read_run_state(workspace: Path) -> dict:
    """Return normalized state and explicitly mark unavailable state."""
    paths = list((workspace / ".bolt" / "sessions").glob("*.json"))
    if not paths:
        return {"_state_available": False}
    try:
        data = json.loads(paths[0].read_text())
        raw = data.get("run_state")
        if not isinstance(raw, dict):
            return {"_state_available": False}
        state = {"_state_available": True}
        for lower_name in _RUN_STATE_FIELDS:
            value = _state_value(raw, lower_name)
            if value is not None:
                state[lower_name] = value
        for field in ("acceptance_criteria_state", "observations", "verification_criteria"):
            if field in state:
                state[field] = _normalize_state_items(state[field])
        return state
    except (OSError, json.JSONDecodeError):
        return {"_state_available": False}


def classify(state: dict, external_ok: bool, bolt_rc: int, provider_error: bool) -> str:
    if provider_error:
        return "FAIL-ENVIRONMENT"
    if external_ok and state.get("phase") == "complete":
        return "PASS"
    if state.get("phase") == "complete" and not external_ok:
        return "FAIL-BOLT"
    if bolt_rc != 0 and not state.get("_state_available", False):
        return "FAIL-TOOL"
    return "FAIL-MODEL"


def evaluate(task: Task, args: argparse.Namespace, root: Path) -> dict:
    started = time.monotonic()
    workspace = Path(tempfile.mkdtemp(prefix=f"bolt-eval-{task.id}-", dir=args.temp_root))
    task_dir = root / task.id
    task_dir.mkdir(parents=True, exist_ok=True)
    stdout_path, stderr_path = task_dir / "bolt.stdout.log", task_dir / "bolt.stderr.log"
    external_path = task_dir / "external-check.log"
    try:
        write_fixture(workspace)
        cmd = [str(args.bolt), "--no-resume", "--no-mcp", "--workspace", str(workspace), "--base-url", args.base_url, "--model", args.model, "--api-key", args.api_key, "exec", task.prompt]
        env = os.environ.copy()
        env["BOLT_PERMISSION_MODE"] = "allow"
        try:
            p = subprocess.run(cmd, cwd=workspace, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=args.timeout)
            bolt_rc = p.returncode
        except subprocess.TimeoutExpired as exc:
            p = type("P", (), {"stdout": exc.stdout or "", "stderr": (exc.stderr or "") + "\nTIMEOUT\n"})
            bolt_rc = 124
        stdout_path.write_text(p.stdout)
        stderr_path.write_text(p.stderr)
        provider_error = bool(re.search(r"(connection refused|context deadline exceeded|HTTP 5\d\d|failed to connect|no such host)", p.stderr + p.stdout, re.I))
        ok, check_summary, check_elapsed = external_check(task, workspace)
        external_path.write_text(check_summary)
        state = read_run_state(workspace)
        session_files = list((workspace / ".bolt" / "sessions").glob("*.json"))
        if session_files:
            shutil.copy2(session_files[0], task_dir / "session.json")
        _, diff, _ = run_checked(["git", "diff", "--stat"], workspace)
        status_code, status, _ = run_checked(["git", "status", "--short"], workspace)
        return {
            "task_id": task.id, "category": task.category, "model": args.model, "provider": args.provider,
            "success": ok and state.get("phase") == "complete", "classification": classify(state, ok, bolt_rc, provider_error),
            "elapsed_seconds": round(time.monotonic() - started, 3), "bolt_exit_code": bolt_rc,
            "run_state_available": state.get("_state_available", False),
            "agent_iterations": state.get("iterations", 0), "tool_calls": state.get("tool_calls_used", 0),
            "retries": state.get("retries", 0), "replans": sum(1 for x in state.get("observations", []) if not x.get("success", True)),
            "verification_attempts": len(state.get("verification_criteria", [])),
            "verification_state": state.get("verification", "not_run"),
            "acceptance_criteria": state.get("acceptance_criteria_state", []), "goal_complete": state.get("phase") == "complete",
            "external_test": {"passed": ok, "summary": check_summary, "elapsed_seconds": round(check_elapsed, 3)},
            "diff_summary": (diff.strip() or status.strip()), "logs": {"stdout": str(stdout_path), "stderr": str(stderr_path), "external": str(external_path), "session": str(task_dir / "session.json") if session_files else ""},
        }
    finally:
        if not args.keep_workspaces:
            shutil.rmtree(workspace, ignore_errors=True)


def self_test() -> None:
    with tempfile.TemporaryDirectory(prefix="bolt-eval-selftest-") as d:
        root = Path(d)
        ws = root / "ws"
        ws.mkdir()
        write_fixture(ws)
        task = task_set()[0]
        (ws / "REPORT.md").write_text("calc/calc.go contains func Add\n")
        assert external_check(task, ws)[0]
        (ws / "REPORT.md").write_text("claimed done\n")
        assert not external_check(task, ws)[0]
        assert not classify({"phase": "complete"}, False, 0, False) == "PASS"
    print("bolt coding eval self-test: PASS")


def endpoint_error(args: argparse.Namespace) -> str:
    try:
        p = subprocess.run([str(args.bolt), "--base-url", args.base_url, "--model", args.model, "--api-key", args.api_key, "--ping"], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=20)
        return "" if p.returncode == 0 else p.stdout[-2000:].strip()
    except (OSError, subprocess.TimeoutExpired) as exc:
        return str(exc)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--bolt", type=Path, default=ROOT / "bolt")
    ap.add_argument("--base-url", default=os.getenv("AGENTERM_BASE_URL", "http://127.0.0.1:11435/v1"))
    ap.add_argument("--model", default=os.getenv("AGENTERM_MODEL", "/sglang-data/models/gpt-oss-120b"))
    ap.add_argument("--api-key", default=os.getenv("AGENTERM_API_KEY", "sglang"))
    ap.add_argument("--provider", default=os.getenv("AGENTERM_PROVIDER", "sglang-s3"))
    ap.add_argument("--timeout", type=int, default=900)
    ap.add_argument("--temp-root", type=Path, default=Path(tempfile.gettempdir()))
    ap.add_argument("--results", type=Path, default=RESULTS)
    ap.add_argument("--keep-workspaces", action="store_true")
    ap.add_argument("--task", action="append", help="run selected task id; repeatable")
    args = ap.parse_args()
    if args.self_test:
        self_test()
        return 0
    if not args.bolt.exists():
        print(f"Bolt binary not found: {args.bolt}", file=sys.stderr)
        return 2
    args.results.mkdir(parents=True, exist_ok=True)
    selected = [x for x in task_set() if not args.task or x.id in args.task]
    unavailable = endpoint_error(args)
    if unavailable:
        results = [{"task_id": task.id, "category": task.category, "model": args.model, "provider": args.provider,
                    "success": False, "classification": "FAIL-ENVIRONMENT", "elapsed_seconds": 0,
                    "bolt_exit_code": None, "agent_iterations": 0, "tool_calls": 0, "retries": 0, "replans": 0,
                    "verification_attempts": 0, "verification_state": "not_run", "acceptance_criteria": [],
                    "goal_complete": False, "external_test": {"passed": False, "summary": "not run: endpoint preflight failed"},
                    "diff_summary": "", "preflight_error": unavailable, "logs": {}} for task in selected]
    else:
        results = [evaluate(task, args, args.results) for task in selected]
    summary = {
        "schema_version": "1.0", "harness": "bolt-coding-eval", "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "model": args.model, "provider": args.provider, "base_url": args.base_url, "tasks": results,
        "preflight": {"passed": not unavailable, "error": unavailable},
        "overall": {"attempted": len(results), "passed": sum(x["classification"] == "PASS" for x in results),
                    "classifications": {k: sum(x["classification"] == k for x in results) for k in ["PASS", "FAIL-BOLT", "FAIL-MODEL", "FAIL-TOOL", "FAIL-ENVIRONMENT", "AMBIGUOUS"]}},
    }
    elapsed = [x["elapsed_seconds"] for x in results]
    iterations = [x["agent_iterations"] for x in results]
    calls = [x["tool_calls"] for x in results]
    summary["metrics"] = {
        "average_elapsed_seconds": statistics.mean(elapsed) if elapsed else 0,
        "median_elapsed_seconds": statistics.median(elapsed) if elapsed else 0,
        "average_iterations": statistics.mean(iterations) if iterations else 0,
        "median_iterations": statistics.median(iterations) if iterations else 0,
        "average_tool_calls": statistics.mean(calls) if calls else 0,
        "median_tool_calls": statistics.median(calls) if calls else 0,
        "total_retries": sum(x["retries"] for x in results),
        "total_replans": sum(x["replans"] for x in results),
        "verification_passed": sum(x["verification_state"] == "passed" for x in results),
    }
    criteria = [criterion for x in results for criterion in x.get("acceptance_criteria", [])]
    summary["metrics"]["acceptance_criteria_satisfied"] = sum(bool(x.get("satisfied")) for x in criteria)
    summary["metrics"]["acceptance_criteria_total"] = len(criteria)
    failures = {}
    for x in results:
        if x["classification"] != "PASS":
            key = x.get("preflight_error", "model did not complete externally checked task")
            failures[key.splitlines()[-1][:160]] = failures.get(key.splitlines()[-1][:160], 0) + 1
    summary["failure_patterns"] = failures
    (ROOT / "test-results").mkdir(exist_ok=True)
    (ROOT / "test-results" / "bolt-coding-eval.json").write_text(json.dumps(summary, indent=2) + "\n")
    by_cat = {}
    for x in results:
        by_cat.setdefault(x["category"], []).append(x["classification"] == "PASS")
    lines = ["# Bolt Coding Eval", "", f"Model: `{args.model}` ({args.provider})", "", f"Completion: {summary['overall']['passed']}/{len(results)}", "", "## By category", ""]
    lines += [f"- {cat}: {sum(vals)}/{len(vals)}" for cat, vals in sorted(by_cat.items())]
    criterion_rate = (summary["metrics"]["acceptance_criteria_satisfied"] / summary["metrics"]["acceptance_criteria_total"] if summary["metrics"]["acceptance_criteria_total"] else 0)
    lines += ["", "## Metrics", "", f"- Acceptance criteria: {summary['metrics']['acceptance_criteria_satisfied']}/{summary['metrics']['acceptance_criteria_total']} ({criterion_rate:.0%})", f"- Verification passed: {summary['metrics']['verification_passed']}/{len(results)}", f"- Average/median elapsed: {summary['metrics']['average_elapsed_seconds']:.1f}s / {summary['metrics']['median_elapsed_seconds']:.1f}s", f"- Average/median iterations: {summary['metrics']['average_iterations']:.1f} / {summary['metrics']['median_iterations']:.1f}", f"- Average/median tool calls: {summary['metrics']['average_tool_calls']:.1f} / {summary['metrics']['median_tool_calls']:.1f}", f"- Retries: {summary['metrics']['total_retries']}; replans: {summary['metrics']['total_replans']}", "", "## Failure patterns", ""]
    lines += [f"- {key}: {count}" for key, count in sorted(failures.items(), key=lambda item: (-item[1], item[0]))] or ["- none"]
    lines += ["", "## Tasks", "", "| Task | Result | Iters | Tools | Retries | Verify |", "|---|---:|---:|---:|---:|---:|"]
    lines += [f"| {x['task_id']} | {x['classification']} | {x['agent_iterations']} | {x['tool_calls']} | {x['retries']} | {x['verification_state']} |" for x in results]
    (ROOT / "test-results" / "bolt-coding-eval.md").write_text("\n".join(lines) + "\n")
    print(json.dumps(summary["overall"], indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
