import json
import tempfile
import unittest
from pathlib import Path

from benchmarks.bolt_coding_eval import read_run_state


class RunStateExtractionTest(unittest.TestCase):
    fixture = Path("/tmp/bolt-agentic-eval-fixture")

    def test_reads_current_go_schema(self):
        state = read_run_state(self.fixture)
        self.assertTrue(state["_state_available"])
        self.assertEqual(state["phase"], "complete")
        self.assertEqual(state["iterations"], 3)
        self.assertEqual(state["tool_calls_used"], 1)
        self.assertEqual(state["retries"], 0)
        self.assertEqual(state["verification"], "passed")

    def test_missing_run_state_is_distinct_from_zero_state(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            session_dir = workspace / ".bolt" / "sessions"
            session_dir.mkdir(parents=True)
            (session_dir / "latest.json").write_text(json.dumps({"meta": {}, "messages": []}))
            state = read_run_state(workspace)

        self.assertFalse(state["_state_available"])
        zero_state = {
            "_state_available": True,
            "iterations": 0,
            "tool_calls_used": 0,
            "verification": "not_run",
        }
        self.assertNotEqual(state["_state_available"], zero_state["_state_available"])


if __name__ == "__main__":
    unittest.main()
