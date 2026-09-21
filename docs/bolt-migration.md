# Bolt migration and freeze record

This records the completed migration from the inspected `boltpy` checkout into
the existing Agenterm Go application. Bolt is now the primary executable and
shared runtime; the Agenterm package/module names remain historical compatibility
identifiers.

| Bolt capability | Agenterm foundation | Migration status |
|---|---|---|
| Provider layer | `internal/llm.Client` and config presets | Reused; Ollama, SGLang, OpenAI-compatible, xAI and custom `/v1` endpoints use the same client. |
| Conversation/tool loop | `internal/agent.Agent` events and history | Reused; streaming and tool-call recovery already exist. |
| Streaming | LLM SSE -> `agent.Event` -> Bubble Tea messages | Reused; token batching and throttled paints are in place. |
| Filesystem/shell/git tools | `internal/tools` registry | Reused and now receives an explicit workspace for core file, git and shell tools. |
| MCP | `internal/mcp.Manager` | Reused; connected tools register in the same registry. |
| Conversation viewport | `internal/tui` Bubbles viewport | Reused; only the viewport scrolls, with follow-at-bottom behavior, keyboard/mouse input, and a fixed responsive todo panel. |
| Session persistence | `Agent.SaveSessionPath` / `LoadSessionPath` | Added for workspace-local `.bolt/sessions/latest.json`; fresh startup is the default and resume is explicit. |
| Permission mode | `internal/permissions` + `agent.EventPermission` | Migrated; ASK/ALLOW/PLAN, safe/confirm/dangerous levels, once/session/permanent grants, deny path, and fixed approval UI. |
| Todos | `internal/todos` + todo tools + fixed TUI panel | Migrated; shared store is independent of conversation scrolling and updates through the existing tool loop. |
| Vision | config/runtime TUI state | Toggle and status are migrated; current Agenterm text client has no image-content transport, so ON is reported as requested-but-unsupported and no image call is fabricated. |
| Bolt footer/shortcut semantics | `internal/tui` model state and Lip Gloss view | Migrated; compact ^Q/^R/^L/^Y/^T/^O/^B controls, Enter, Shift+Enter, active-run timer, dynamic model/status/token display, fixed panels, and visible cursor block. |
| `bolt`, `bolt-s1` … `bolt-s8` launchers | one executable selected by `argv[0]` | Thin shared-runtime aliases. They select only launcher behavior; model, endpoint, provider, and API key come from the loaded config or supported environment overrides. S1–S3 retain their behavioral defaults (ALLOW, with S3 visible-answer mode). |
| Upgrade | historical `bolt upgrade` installer command | Restored as `bolt upgrade`; it checks the latest `boltgo` GitHub release, verifies published SHA-256 assets, and atomically replaces only the executable. Configuration, permissions, sessions, and workspace files are untouched. Existing `agenterm-*` release asset names remain supported for compatibility. |

## Current execution contract

`--workspace` or `BOLT_WORKSPACE` selects the user workspace. It is separate
from the executable/source directory and is passed to the core file, git and
shell runners. A normal launch starts with only the system message. The
workspace session is loaded only for `--resume` or truthy `BOLT_RESUME`; an
explicit `--no-resume` wins over the environment.

No further migration work is required for the implemented scope. Future
multimodal work is optional and separately scoped.

SSH config aliases are resolved from the user's `~/.ssh/config` through the
`ssh_execute` tool. Workspace file tools do not inspect `.ssh/config`; remote
image inventory commands are normalized to non-interactive root commands so a
request such as “go to podman9 and list Docker images” remains read-only and
does not depend on an interactive shell.

MCP coding integrations use `[[mcp_servers]]` entries in
`~/.agenterm/config.toml`. Set `enabled = true` and provide either a local
`command` plus `args` for stdio or a `streamable_http` `url`. For HTTP auth,
set `auth_env` to the name of an environment variable containing the bearer
token; credentials are not persisted. Discovered tools are registered as
`<server>__<tool>` and participate in the same agent loop as built-in tools.

## Phase 4 parity and hardening assessment

### Complete and tested

- Fresh/resume session isolation, including provider-request context tests.
- Workspace-local project rules and workspace-local session persistence.
- Protection against overwriting a missing or corrupt explicitly requested session.
- Shared permission state is mutex-protected during concurrent UI/agent activity.
- Stream completion closes the active turn before starting the next queued
  request; the input editor remains available while the turn runs.
- Slash session commands (`/save`, `/load`, `/sessions`) and `/status` use the
  selected Bolt workspace rather than legacy global Agenterm state.
- Ollama/S1 streaming, token accounting, workspace file creation, validation,
  provider failure, and tool failure were exercised.
- Window-size and mouse-wheel model handling have deterministic tests.
- Session storage rejects traversal, absolute session IDs, and symlinked
  `.bolt/sessions` directories that resolve outside the selected workspace.

Tool paths intentionally accept explicit absolute paths, and shell/git/test
commands execute with the selected workspace as their cwd. This is a
workspace-routing boundary, not a filesystem sandbox for arbitrary commands;
the permission policy remains the approval boundary for mutating tools.

### Implemented but environment-dependent validation

- Physical mouse-wheel behavior in SELECT and INTERACTIVE terminal modes.
- Physical terminal resize during long streaming responses and open panels.
- S2/S3 live provider requests. Their configured local endpoints were
  unavailable during this validation; no external service was changed.

### Intentionally deferred

- Vision image transport. The current provider-neutral message remains
  text-only, so Ctrl+Y is state-only and reports that image capability is not
  available. Adding fake image support would misrepresent provider behavior.
- S2/S3 tunnel startup/model warm-up. The shared Go launchers select endpoint
  and model; external shell-managed lifecycle remains outside the executable.

## Phase 3 runtime validation

The existing local Ollama endpoint at `http://127.0.0.1:11435/v1` was tested
with real streaming requests through `bolt-s1` and the TUI. The exact
`BOLT-LIVE-TEST` response was received incrementally. A real `write_file`
request displayed the blocking approval panel; allow-once created the expected
workspace file, and deny produced a visible tool error without creating the
requested file. The TUI returned to Ready and remained usable after both
paths. Provider-reported usage is now propagated to the footer; providers
without usage continue to display `Tokens: —`.

Headless flags are persistent across subcommands, so both
`bolt --workspace ... exec ...` and `bolt exec --workspace ...` use the same
workspace/session policy. Explicit headless resume and `--no-resume` over an
active `BOLT_RESUME=1` environment were verified against a temporary
workspace.

The current S2/S3 follow-up used only the existing local Ollama-compatible
endpoints and did not download or switch models. The podman9 image-inventory
query was verified through S3 at `127.0.0.1:30004`; the wrappers remain
endpoint/model selectors and do not manage externally controlled tunnels or
model warm-up.

The PTY smoke verified live streaming, token/status updates, approval/deny,
^Q/^R/^L/^Y/^T/^O/^B, fixed input/footer/workspace, active-run timing, visible
cursor block, and clean teardown. Full
mouse-wheel traversal and terminal-resize behavior still need a terminal
environment that can generate wheel/resize events; the canonical Bubbles
viewport remains in place and the automated state tests pass. The conversation
viewport now routes plain Up/Down when the input is empty, preserves scroll
intent during streaming, and keeps todos in a fixed right-side panel at normal
widths with a narrow vertical fallback.

## Permission behavior

Interactive Bolt starts in `ASK` mode. Safe reads and todo operations run
without a prompt. Confirm-level writes, shell, tests, git mutations, and SSH
operations emit a blocking approval event. The fixed approval view supports
`1` allow once, `2` allow for the session, `3` allow permanently, and `4` deny.
Dangerous operations always require a fresh allow-once decision. Permanent
grants are stored in `~/.config/bolt/permissions.json`; `BOLT_PERMISSION_MODE`
can select `ask`, `allow`, or `plan`.

## Launcher compatibility

`make build` creates one `bolt` binary and `bolt-s1` through `bolt-s8`
symlinks. The executable selects its behavioral preset from `argv[0]`; all
paths use the same runtime, workspace handling, session policy, and TUI. Model,
endpoint, provider, and API key values are resolved centrally from the config
file and supported `BOLT_S*_BASE_URL`, `BOLT_S*_MODEL`, and `BOLT_S*_API_KEY`
environment overrides. Tunnel lifecycle remains the caller's responsibility.
