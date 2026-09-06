# Bolt — Go Terminal Coding Agent (Ollama, SGLang, OpenAI, xAI + MCP Client)

Bolt is the primary Go implementation in this repository. It reuses the
historical Agenterm package/module foundation; `agenterm` references below are
kept where they describe that compatibility foundation, module path, or
existing tooling.

**Terminal AI agent** and **coding assistant in the terminal** for [Ollama](https://ollama.com), [SGLang](https://github.com/sgl-project/sglang), [xAI](https://x.ai), [OpenAI](https://openai.com), and any **OpenAI-compatible** API. Full-screen **TUI**, optional **file/shell tools**, and **[Model Context Protocol (MCP)](https://modelcontextprotocol.io)** client support—one static **Go** binary.

> **Lab 1** in the [AI · Agents · MCP learning path](https://github.com/saurabhahuja71/learning-path#7-ai--agents--mcp) · Audience: intermediate · Time: ~1 hour to first chat · Level: intermediate

**SEO keywords:** *terminal AI agent*, *Ollama TUI*, *SGLang OpenAI-compatible*, *OpenAI compatible CLI agent*, *MCP client Go*, *local LLM coding agent*, *agenterm tutorial*, *AI pair programmer terminal*.

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![Ollama](https://img.shields.io/badge/Ollama-compatible-black)](https://ollama.com)
[![SGLang](https://img.shields.io/badge/SGLang-compatible-orange)](https://github.com/sgl-project/sglang)
[![MCP](https://img.shields.io/badge/MCP-client-purple)](https://modelcontextprotocol.io)
[![Release](https://img.shields.io/github/v/release/saurabhahuja71/boltgo?include_prereleases)](https://github.com/saurabhahuja71/boltgo/releases)

| | |
|---|---|
| **Best for** | Developers who want Ollama, SGLang, or any `/v1` chat API in the terminal with optional coding tools |
| **Runs on** | Linux, macOS, Windows · Docker / Podman |
| **Not** | A web UI or desktop app — pure terminal |

```
You (TUI)
   │
   ▼
bolt  ──►  Ollama / SGLang / xAI / OpenAI  (POST /v1/chat/completions)
   │
   └── tools ──► built-in (files, optional shell) + MCP servers
```

---

## Get started in 60 seconds

**1. Install** (Linux / macOS):

```bash
curl -fsSL https://raw.githubusercontent.com/saurabhahuja71/boltgo/main/scripts/install.sh | bash
```

Binary goes to `~/.local/bin/agenterm`. If the shell cannot find it:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

**2. Start a backend** (pick one):

```bash
# Ollama (default)
ollama pull qwen2.5-coder:32b
ollama serve   # http://127.0.0.1:11434

# or SGLang (OpenAI-compatible on :30000 — see SGLang section below)
# sglang serve --model-path /path/to/model.gguf --quantization gguf \
#   --host 127.0.0.1 --port 30000 --served-model-name qwen2.5-coder-32b-q4_k_m.gguf
```

**3. Chat with Bolt:**

```bash
bolt init              # first time: writes ~/.agenterm/config.toml
bolt --ping            # check the API
bolt                   # open the Bolt TUI (Ollama default)

# Shared launcher presets:
bolt-s1                # configured Ollama/S1 runtime
bolt-s2                # configured SGLang/S2 runtime
bolt-s3                # configured SGLang/S3 runtime

# SGLang instead:
bolt --provider sglang --ping
bolt --provider sglang
```

In the TUI: type a message and press **Enter**. Try `/help`, `/model`, or `/tools off` for pure chat.

---

## Table of contents

- [Why agenterm?](#why-agenterm)
- [Install](#install)
- [Quick start](#quick-start)
- [Local vs remote Ollama](#local-vs-remote-ollama)
- [SGLang](#sglang)
- [Docker / Podman](#docker--podman)
- [CLI and in-chat commands](#cli-and-in-chat-commands)
- [Configuration](#configuration)
- [Tools and MCP](#tools-and-mcp)
- [FAQ](#faq)
- [Architecture](#architecture)
- [Releases](#releases)
- [License](#license)

---

## Why Bolt?

| You want… | Bolt does… |
|-----------|----------------|
| Chat with **local LLMs** | Defaults to Ollama at `http://127.0.0.1:11434/v1` |
| Faster **SGLang** serving | `--provider sglang` → `http://127.0.0.1:30000/v1` |
| A **GPU box over the network** | `--base-url` or an SSH tunnel to localhost |
| A **coding agent** in the shell | Tool loop: list/read/write files; optional shell; MCP |
| Switch models mid-session | `/model` while chatting |
| Fast small talk | Greetings skip tools; use `--no-tools` for pure chat |
| No Python/Node runtime | One **static Go binary** |

Not affiliated with xAI. “Grok-style” only means a snappy terminal agent UX.

---

## Install

### One-line (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/saurabhahuja71/boltgo/main/scripts/install.sh | bash
agenterm --version   # compatibility installer name; source builds produce bolt
```

| Variable | Meaning |
|----------|---------|
| `INSTALL_DIR` | Install path (default `~/.local/bin`) |
| `AGENTERM_VERSION` | Release tag or `latest` |
| `AGENTERM_FROM_SOURCE=1` | Build with Go instead of downloading a release |
| `AGENTERM_SKIP_INIT=1` | Do not create config |

### GitHub Releases

```bash
curl -fsSL -o agenterm \
  https://github.com/saurabhahuja71/boltgo/releases/latest/download/agenterm-linux-amd64
chmod +x agenterm && ./agenterm
```

Also published: `linux-arm64`, `darwin-amd64`, `darwin-arm64`, `windows-amd64.exe`.

### From source

```bash
git clone https://github.com/saurabhahuja71/boltgo.git
cd boltgo
make build          # → ./bolt and bolt-s1/bolt-s2/bolt-s3 aliases
./bolt              # primary Go Bolt executable
make install        # → $(go env GOPATH)/bin (or $GOBIN)
# or:
go install github.com/saurabhahuja71/agenterm/cmd/agenterm@latest
```

---

## Quick start

### Ollama on this machine

```bash
ollama pull qwen2.5-coder:32b
bolt --ping
bolt -m qwen2.5-coder:32b
# pure chat (no function tools):
bolt --no-tools -m qwen2.5-coder:32b
```

### Ollama on another host (SSH tunnel)

```bash
ssh -N -L 11434:127.0.0.1:11434 user@gpu-host
# Bolt still uses http://127.0.0.1:11434/v1
bolt
```

### SGLang (local or tunneled)

SGLang exposes the same OpenAI-compatible `/v1/chat/completions` surface. Default port in this project’s docs: **30000**.

```bash
# Server already on this machine (or tunnel already open):
bolt --provider sglang --ping
bolt --provider sglang
bolt --provider sglang --no-tools   # pure chat
```

SSH tunnel from a laptop to a GPU host:

```bash
ssh -N -L 30000:127.0.0.1:30000 user@gpu-host
# or with a jump host:
# ssh -N -J bastion -L 30000:127.0.0.1:30000 opc@gpu-host
bolt --provider sglang
```

Model id must match SGLang’s **`--served-model-name`** (often the GGUF basename). List with:

```bash
curl -s http://127.0.0.1:30000/v1/models
# then:
bolt --provider sglang -m qwen2.5-coder-32b-q4_k_m.gguf
```

### In the TUI

| Action | How |
|--------|-----|
| Send message | **Enter** |
| Help | `/help` |
| List / switch model | `/model` · `/model qwen2.5-coder:32b` |
| Disable tools this session | `/tools off` |
| Clear history | `/clear` or **Ctrl+L** |
| Quit | **Ctrl+C** |

Run Bolt with an explicit workspace (`bolt --workspace /path/to/myrepo`) so file tools use that workspace.

---

## Local vs remote Ollama

### Config (`~/.agenterm/config.toml`)

```toml
provider = "ollama-local"
model = "qwen2.5-coder:32b"
base_url = "http://127.0.0.1:11434/v1"
api_key = "ollama"
```

Remote host on the LAN:

```toml
provider = "ollama-remote"

[providers.ollama-remote]
base_url = "http://192.168.1.50:11434/v1"
api_key = "ollama"
model = "qwen2.5"
```

### One-shot CLI

```bash
bolt --base-url http://127.0.0.1:11434/v1 -m qwen2.5-coder:32b
bolt --base-url http://gpu-box:11434/v1 -m qwen2.5

export XAI_API_KEY=xai-...
bolt --provider xai -m grok-3

export OPENAI_API_KEY=sk-...
bolt --provider openai -m gpt-4o-mini
```

Always include **`/v1`** on Ollama base URLs (`/v1/chat/completions`, `/v1/models`).

Full example: [`configs/config.example.toml`](configs/config.example.toml).

---

## SGLang

[SGLang](https://github.com/sgl-project/sglang) is an OpenAI-compatible inference server (often used with GGUF / multi-GPU). agenterm talks to it the same way as Ollama: `POST {base_url}/chat/completions`.

| | Ollama (default) | SGLang |
|--|------------------|--------|
| Typical URL | `http://127.0.0.1:11434/v1` | `http://127.0.0.1:30000/v1` |
| Provider preset | `ollama-local` / `ollama-remote` | `sglang` |
| Model id | Ollama tag (`qwen2.5-coder:32b`) | `--served-model-name` (e.g. GGUF basename) |
| API key | any string (e.g. `ollama`) | any string (e.g. `sglang`) |

### Config (`~/.agenterm/config.toml`)

```toml
provider = "sglang"
# optional top-level overrides; preset also sets these under [providers.sglang]

[providers.sglang]
base_url = "http://127.0.0.1:30000/v1"
api_key = "sglang"
# Must match the id returned by GET /v1/models
model = "qwen2.5-coder-32b-q4_k_m.gguf"
```

Or keep Ollama as default and only switch on the CLI:

```bash
bolt --provider sglang
bolt --base-url http://127.0.0.1:30000/v1 -m qwen2.5-coder-32b-q4_k_m.gguf --api-key sglang
```

Env equivalent:

```bash
export AGENTERM_PROVIDER=sglang
export AGENTERM_BASE_URL=http://127.0.0.1:30000/v1
export AGENTERM_MODEL=qwen2.5-coder-32b-q4_k_m.gguf
export AGENTERM_API_KEY=sglang
bolt --ping && bolt
```

### Start SGLang (sketch)

On the GPU host (adjust paths, tensor-parallel size, and context to your hardware):

```bash
# Example: dense Qwen2.5 coder GGUF, OpenAI API on localhost:30000
sglang serve \
  --model-path /path/to/qwen2.5-coder-32b-q4_k_m.gguf \
  --quantization gguf \
  --host 127.0.0.1 \
  --port 30000 \
  --tp-size 2 \
  --context-length 32768 \
  --served-model-name qwen2.5-coder-32b-q4_k_m.gguf \
  --trust-remote-code
```

Prefer **dense** GGUF weights for stability. Some **MoE** GGUFs (e.g. certain qwen3-coder builds) can fail at runtime depending on the SGLang build—use Ollama for those tags if needed.

### Remote GPU + SSH tunnel

SGLang is often bound to **`127.0.0.1` only** on the GPU box (no public ingress). From your workstation:

```bash
ssh -N -L 30000:127.0.0.1:30000 user@gpu-host
# jump host:
# ssh -N -J bastion -L 30000:127.0.0.1:30000 opc@gpu-host

curl -s http://127.0.0.1:30000/v1/models
bolt --provider sglang --ping
bolt --provider sglang
```

If you use a corporate HTTP proxy, exclude localhost so the tunnel is not proxied:

```bash
export no_proxy="localhost,127.0.0.1,${no_proxy:-}"
export NO_PROXY="localhost,127.0.0.1,${NO_PROXY:-}"
```

### Ollama vs SGLang on the same GPUs

Do **not** load a heavy Ollama model and a heavy SGLang model on the same GPUs at once (memory contention). Unload Ollama keep-alives before starting SGLang, or stop the SGLang service before `ollama run` of a large tag.

| Goal | Suggested path |
|------|----------------|
| Everyday chat / tags from `ollama list` | Ollama · `agenterm` (default) |
| Lower-latency GGUF serve, multi-GPU TP | SGLang · `bolt --provider sglang` |

Always include **`/v1`** on the base URL (`/v1/chat/completions`, `/v1/models`).

---

## Docker / Podman

Prefer the **native binary** for daily use. Use a container when you want a locked-down runtime.

```bash
podman run --rm -it --network=host \
  -e AGENTERM_BASE_URL=http://127.0.0.1:11434/v1 \
  -v "$HOME/.agenterm:/home/agenterm/.agenterm:Z" \
  ghcr.io/saurabhahuja71/agenterm:latest
```

From this repo:

```bash
./scripts/run-podman.sh --ping
./scripts/run-podman.sh
```

| Need | Why |
|------|-----|
| `-it` / TTY | Interactive TUI |
| `--network=host` (Linux) | Reach host Ollama (`:11434`) or SGLang (`:30000`) |
| Volume `~/.agenterm` | Persist config |

On **macOS Docker Desktop**, use `http://host.docker.internal:11434/v1` for host Ollama (or `:30000` for host SGLang).

```bash
export DOCKER_HOST=unix:///run/user/$(id -u)/podman/podman.sock   # rootless Podman
podman compose run --rm agenterm
```

---

## CLI and in-chat commands

### Flags

```bash
bolt                      # open TUI
bolt init                 # default config (--force to overwrite)
bolt --ping               # connectivity check
bolt -m qwen2.5           # start with a model
bolt --provider ollama-remote
bolt --provider sglang
bolt --base-url http://host:11434/v1
bolt --base-url http://127.0.0.1:30000/v1 -m qwen2.5-coder-32b-q4_k_m.gguf
bolt --no-tools           # pure chat (faster)
bolt --shell              # allow run_shell
bolt --no-mcp
bolt upgrade              # download and atomically install the latest Go release
```

### In-chat

```text
/help
/status
/model                         # list models (* = current)
/model qwen2.5-coder:32b       # switch model
/models                        # alias for /model
/tools on | /tools off
/clear
/quit
```

Greetings skip tools automatically. For fully tool-free sessions: `/tools off`, `enable_tools = false`, or `AGENTERM_ENABLE_TOOLS=0`.

---

## Configuration

| Env | Meaning |
|-----|---------|
| `AGENTERM_BASE_URL` | API root (e.g. `http://127.0.0.1:11434/v1` or `http://127.0.0.1:30000/v1`) |
| `AGENTERM_MODEL` | Model id / Ollama tag / SGLang served name |
| `AGENTERM_PROVIDER` | `ollama-local`, `ollama-remote`, `sglang`, `xai`, `openai`, `custom` |
| `AGENTERM_ENABLE_TOOLS` | `0`/`false` off · `1`/`true` on |
| `AGENTERM_API_KEY` / `OLLAMA_API_KEY` / `XAI_API_KEY` / `OPENAI_API_KEY` | Auth |
| `AGENTERM_CONFIG` | Path to config file |

Default file: `~/.agenterm/config.toml`.

---

## Tools and MCP

### Built-in tools

| Tool | Notes |
|------|--------|
| `list_dir` | List a directory (relative to process cwd) |
| `read_file` | Read a file |
| `write_file` | Write a full file |
| `str_replace` | Surgical edit (preferred for patches) |
| `find_files` | Locate files when the path is unknown |
| `git` | Allowlisted git (`status`, `checkout`, `add`, `commit`, …) |
| `run_shell` | Off by default; `--shell` or `enable_shell = true` |

When you ask to **apply / implement / do it**, Bolt uses tools—not only printed shell recipes.

### MCP

Bolt is an **MCP client**. Example:

```toml
[[mcp_servers]]
name = "mcp-demo"
enabled = true
url = "http://127.0.0.1:8080/mcp"
```

Disable for a session: `bolt --no-mcp`. Demo server: [mcp-demo](https://github.com/saurabhahuja71/mcp-demo).

---

## FAQ

### What is Bolt?

An open-source **terminal AI agent** in Go: a full-screen TUI that streams chat from Ollama, SGLang, or any OpenAI-compatible API, with optional tools (files, shell, MCP).

### Is it an Ollama GUI?

No—it is a **terminal TUI**, not a web or desktop GUI. Use it when you want chat and agent tools without leaving the shell.

### Can I use remote Ollama or an SSH tunnel?

Yes. Point `base_url` / `--base-url` at the host, or tunnel remote Ollama to `127.0.0.1:11434` and keep the default URL.

### Does it work with SGLang?

Yes. `--provider sglang` (default `http://127.0.0.1:30000/v1`), or any custom base URL that exposes `/v1/chat/completions`. Use the **served model name** from `GET /v1/models`, not the Ollama tag, unless you set them to match. See [SGLang](#sglang).

### Does it work with xAI Grok and OpenAI?

Yes. `--provider xai` or `--provider openai` with the matching API key env vars, or any custom `--base-url` that speaks OpenAI chat completions.

### How do I change the model during a chat?

`/model` to list, then `/model <name>` (for example `/model qwen2.5-coder:32b`).

### Why was “hi” slow before?

Some models called tools (e.g. `list_dir`) on greetings. Trivial chat now skips tools; use `--no-tools` or `/tools off` for pure chat.

### Is Python required?

No for release binaries. Bolt is a **static Go binary**. The historical
`agenterm` installer/module naming remains for compatibility.

### Where is the config?

Default: `~/.agenterm/config.toml`. Override with `AGENTERM_CONFIG` or `--config`.

### Is this related to xAI Grok?

No affiliation. Independent project.

---

## Architecture

```
cmd/agenterm/          CLI entry
internal/config/       TOML + env + provider presets
internal/llm/          OpenAI-compatible client (stream + tools)
internal/agent/        Multi-turn tool loop
internal/tools/        Built-in tools
internal/mcp/          MCP client
internal/tui/          Bubble Tea UI
scripts/install.sh     End-user installer
```

| Piece | Tech |
|--------|------|
| Language | Go |
| CLI | [Cobra](https://github.com/spf13/cobra) |
| TUI | [Bubble Tea](https://github.com/charmbracelet/bubbletea) · Bubbles · Lip Gloss |
| Markdown | [Glamour](https://github.com/charmbracelet/glamour) |
| MCP | [official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) |

Chat **replies** come from the LLM. MCP only supplies **tools**. Models are **not** embedded in the binary.

More detail: [`docs/how-it-works.md`](docs/how-it-works.md).  
Roadmap (Grok-class UX): [`docs/grok-parity-roadmap.md`](docs/grok-parity-roadmap.md).

**Notes:** Ollama and SGLang must expose the OpenAI-compatible API (default for both). Tool quality depends on the model (`qwen2.5`, `llama3.1+` work better than tiny models).

---

## Releases

On version tags, GitHub Actions publishes multi-platform binaries and a GHCR image:

| Asset | Platforms |
|-------|-----------|
| `agenterm-linux-amd64` | Linux x86_64 |
| `agenterm-linux-arm64` | Linux aarch64 |
| `agenterm-darwin-amd64` | macOS Intel |
| `agenterm-darwin-arm64` | macOS Apple Silicon |
| `agenterm-windows-amd64.exe` | Windows |
| `ghcr.io/saurabhahuja71/agenterm:vX.Y.Z` | Container |

```bash
git tag v0.2.1 && git push origin v0.2.1
make dist VERSION=0.2.1   # local cross-build
```

---



## Learning path — AI / Agents / MCP

| # | Lab | Focus |
|---|-----|--------|
| 0 | [mcp-demo](https://github.com/saurabhahuja71/mcp-demo) | Build MCP server tools |
| **1 (this)** | agenterm | Terminal agent + MCP client |
| 2 | [agentic-ai-sample](https://github.com/saurabhahuja71/agentic-ai-sample) | LangChain ReAct research agent |
| 3 | [workshop-redis-ai](https://github.com/saurabhahuja71/workshop-redis-ai) | Redis vector search + RAG workshop |

Hub: [learning-path](https://github.com/saurabhahuja71/learning-path)

## Topics / SEO tags

`ai-agent` `ollama` `mcp` `golang` `tui` `openai` `xai` `terminal` `llm` `coding-agent` `tutorial` `education`


## License

MIT — free to use, modify, and distribute.

---

## GitHub topics (maintainers)

Suggested topics for the About sidebar:

`ai` · `terminal` · `cli` · `tui` · `ollama` · `sglang` · `llm` · `openai` · `mcp` · `agent` · `golang` · `bubbletea` · `coding-assistant` · `local-llm`
