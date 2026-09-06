# Targeted Bolt tool-call task

Instruction:

> Inspect the repository and summarize the implementation of `main.py`.

Acceptance is evaluated by the normal Bolt agent path: the model must request
`read_file` with one JSON object containing `path=main.py`, receive the file,
and continue to a summary. The task is valid for both a complete tool call in
one response and streamed argument fragments such as `{"pa` followed by
`th":"main.py"}`. A provider chunk is never treated as a complete argument
object until the per-call buffer is complete.

Deterministic coverage lives in:

- `internal/llm/client_test.go`: direct/non-stream shape and split-stream
  parsing, including multiple calls and identity/index routing.
- `internal/agent/agent_test.go`: exact multi-call dispatch for the production
  prompt shape, including continuation after one tool failure.

This task intentionally does not change prompts or special-case `main.py`.
