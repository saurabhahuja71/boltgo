# Continuity

- Summary: Released v1.1.18. Codex-like TUI UX polish (content-first conversation, compact status, borderless input, subtler Todo) plus `read_file · path required` fix via cumulative-safe streamed argument merging and stronger path-tool argument normalization.
- Files modified: `internal/tui/app.go`, `internal/tui/theme_fill_test.go`, `internal/tui/bolt_state_test.go`, `internal/llm/client.go`, `internal/llm/client_test.go`, `internal/agent/agent.go`, `internal/agent/toolcall_text_test.go`, `cmd/agenterm/main.go`, `Makefile`, and this file.
- Decisions: Quiet terminal space over full-bleed fills. Path remains required; empty `{}` still errors. Cumulative SSE tool argument frames must not duplicate via plain `+=`.
- Checks: Full Go tests, vet, build, make build, and race tests for agent/llm/tui pass with `GOCACHE=/tmp/agenterm-go-build`.
- TODOs/blockers: Physical MATE eyeball of Light/Dark UX after upgrade. Live model-backed read_file still depends on a healthy Ollama model endpoint.
- Freeze state: migration remains frozen; installer points to `saurabhahuja71/boltgo`; launchers `bolt` / `bolt-s1` / `bolt-s2` / `bolt-s3` unchanged. Current release baseline is 1.1.18. Module path remains `github.com/saurabhahuja71/agenterm`.
