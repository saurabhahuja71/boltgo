# How Bolt works

Short overview for new users and contributors. For install and day-to-day use, start with the [README](../README.md).

## Big picture

Bolt is a **terminal client**, not a model host:

1. You type in a **TUI** (Bubble Tea).
2. The **agent loop** sends history to an OpenAI-compatible chat API.
3. The model may stream text and/or **tool calls**.
4. Built-in tools (and optional **MCP** tools) run and return results.
5. Results go back into the loop until the model finishes with a normal reply.

```
┌─────────────────────────────────────────────┐
│  TUI (Bubble Tea)                           │
│  scrollback · input · /commands             │
└─────────────────┬───────────────────────────┘
                  │ user message
                  ▼
┌─────────────────────────────────────────────┐
│  Agent loop                                 │
│  history → LLM → tokens / tool_calls        │
│            ↑_______________|                │
│            tool results                     │
└───────┬─────────────────────┬───────────────┘
        │                     │
        ▼                     ▼
  Built-in tools         MCP client
  list/read/write        mcp-demo, …
        │
        ▼
  OpenAI-compatible API
  Ollama · SGLang · xAI · OpenAI · …
```

## Why OpenAI-compatible?

Local stacks (Ollama, SGLang, vLLM, LocalAI) and many clouds speak:

`POST {base_url}/chat/completions`

One client covers:

| Target | Example `base_url` |
|--------|---------------------|
| Local Ollama | `http://127.0.0.1:11434/v1` |
| Remote Ollama | `http://192.168.1.50:11434/v1` |
| SGLang (local / SSH tunnel) | `http://127.0.0.1:30000/v1` |
| xAI | `https://api.x.ai/v1` |
| OpenAI | `https://api.openai.com/v1` |

Always include **`/v1`** so `/v1/chat/completions` and `/v1/models` work (Ollama and SGLang both use this layout).

**SGLang notes:** model id is the server’s `--served-model-name` (often a GGUF basename), not necessarily an Ollama tag. Preset: `provider = "sglang"`. See [README · SGLang](../README.md#sglang).

## Config resolution order

Later sources win:

1. Built-in defaults  
2. `~/.agenterm/config.toml` (or `AGENTERM_CONFIG`)  
3. Provider preset (`providers.<name>`)  
4. Environment variables  
5. CLI flags  

## Chat vs tools vs MCP

| Piece | Role |
|-------|------|
| **LLM** | Streams the assistant reply (and optional tool calls) |
| **Built-in tools** | Files, git, optional shell — run on your machine |
| **MCP** | Extra tools from external servers; agenterm is the **client** |

Models are **not** bundled in the binary. You need Ollama, SGLang, or another API running somewhere reachable.

## Quiet by default

- Greetings and trivial chat skip tools (fewer round-trips).
- Full tool dumps stay off unless you enable verbose mode (`/verbose` when available).
- Pure chat: `bolt --no-tools` or `/tools off`.

## Related

- Roadmap and Grok-class UX gaps: [`grok-parity-roadmap.md`](grok-parity-roadmap.md)
- Example config: [`../configs/config.example.toml`](../configs/config.example.toml)
