# Zoho MCP prototype

## Architecture assessment

Bolt already consumes external MCP servers through `internal/mcp/client.go`.
Configured command servers use stdio through the SDK's `CommandTransport`.
Configured URL servers already use the SDK's `StreamableClientTransport`, so
no new MCP wire transport was necessary.

The existing flow is:

```text
config -> MCP client initialize -> tools/list -> tools.Registry -> agent tool call -> tools/call
```

MCP tools are exposed with the stable local name `<server>__<tool>`. Tool
arguments remain JSON-structured, and MCP tool errors remain errors instead of
being reported as successful results. Session close is handled by the existing
manager lifecycle.

## Zoho requirements verified

The official Zoho MCP landing page describes Zoho MCP as model-agnostic,
permission-scoped, and OAuth-authorized:

<https://www.zoho.com/mcp/>

The public page does not publish a universal external MCP endpoint, a fixed
endpoint path, or a static token. The endpoint and authenticated access token
must therefore come from the Zoho MCP setup for the user's account. This
prototype does not invent those values and does not implement a fake browser
OAuth flow.

The Go MCP SDK already supports Streamable HTTP and has OAuth handler hooks.
Bolt currently uses a controlled bearer-token prerequisite instead: the token
is read from an environment variable at connection time and is never written
to configuration or diagnostics. Interactive OAuth can be added later once the
Zoho endpoint's documented authorization metadata and callback requirements are
available.

## Configuration

Bolt keeps its existing TOML array style. Use a runtime URL and token:

```toml
[[mcp_servers]]
name = "zoho"
enabled = true
transport = "streamable_http"
url = "${ZOHO_MCP_URL}"
auth_env = "ZOHO_MCP_ACCESS_TOKEN"
```

Optional non-secret headers can be supplied with `headers = { ... }`; header
values also support environment expansion. Do not put OAuth access tokens in
the TOML file.

Before starting Bolt, the user must obtain the Zoho MCP endpoint and token
through Zoho's documented authenticated setup:

```bash
export ZOHO_MCP_URL='https://...'
export ZOHO_MCP_ACCESS_TOKEN='...'
bolt
```

The actual values are intentionally omitted from this repository and from
test artifacts.

## Safety boundary

This change adds no Zoho-specific tools and no write filtering. The first live
test must use a Zoho MCP configuration exposing only read-only capabilities.
Bolt treats the server's discovered tools generically; Zoho write safety must
be enforced by the configured Zoho MCP server/account until a separately
approved write-operation phase.

## Current validation

Deterministic tests use a local mock Streamable HTTP MCP server and verify:

- TOML transport configuration;
- environment-expanded URL and bearer token injection;
- missing URL and missing token errors;
- initialization and dynamic tool discovery;
- structured argument preservation;
- result and MCP error preservation;
- bounded connection timeout;
- no token in configuration errors.

No live Zoho smoke test was run because no documented endpoint/token was
provided to the workspace. Stdio transport code was left unchanged.
