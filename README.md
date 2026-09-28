# codexify-go

Experimental Go implementation of the Codexify runtime architecture, starting
with the part that most benefits from native Windows integration: service
lifecycle and supervision of OpenAI's official Secure MCP Tunnel runtime.

The project is inspired by
[devnoname120/codexify](https://github.com/devnoname120/codexify), which is MIT
licensed. This repository is an independent implementation, not a line-by-line
port and not yet a drop-in replacement.

## Current status

Implemented:

- native Windows Service Control Manager (SCM) integration;
- automatic Windows service startup;
- SCM recovery actions at 5s, 15s and 60s;
- no Task Scheduler or PowerShell in the service lifecycle;
- supervision of the official `tunnel-client-runtime`;
- exponential child restart backoff;
- health monitoring through the tunnel runtime `health.url` + `/readyz`;
- stale health-file removal before every tunnel start;
- controlled process-tree shutdown with Windows fallback to forced termination;
- `CREATE_NO_WINDOW` for child processes;
- JSON structured logging;
- foreground mode for debugging;
- `doctor` checks;
- unit and Windows process integration tests;
- official MCP Go SDK (`modelcontextprotocol/go-sdk` v1.7.0);
- stateless Streamable HTTP MCP transport with current-protocol negotiation and
  backwards compatibility;
- localhost-only MCP endpoint validation and request-size limits;
- generated per-process bearer authentication between the tunnel runtime and
  local MCP server (the secret is not stored in config);
- `/health` endpoint;
- workspace-root confinement, including traversal and symlink-boundary checks;
- `get_environment`;
- `read_file` / `write_file`;
- recursive `glob` and regex `grep`;
- `exec_command` with long-running sessions and `write_stdin`;
- `git_status`, `git_diff`, and `git_log`;
- Windows user-context worker launched from the SCM service with
  `CreateProcessAsUser`;
- active WTS session discovery (console or RDP-compatible active session);
- worker Job Object with `KILL_ON_JOB_CLOSE`, so worker child processes do not
  survive service shutdown;
- independent worker and tunnel supervision/restart loops;
- MCP tools and shell commands execute as the logged-in user rather than
  `LocalSystem`;
- multi-project catalogue below one configured access root, including explicit
  project metadata and bounded filesystem discovery;
- durable ChatGPT conversation binding keyed by a SHA-256 digest of
  `_meta["openai/session"]`; the raw conversation id is never persisted;
- `list_projects`, `set_project_root`, and `list_worktrees`;
- immutable per-conversation project selection with idempotent repeated
  selection;
- managed Git worktrees with `auto`, `always`, and `never` policies; `auto`
  isolates the second active conversation that selects the same source project;
- upstream MCP aggregation for stdio and Streamable HTTP servers;
- upstream `catalog` mode through `mcp_list_sources`, `mcp_search_tools`,
  `mcp_get_tool`, and `mcp_call_tool`, plus `direct` mode using
  `<source>__<tool>` names;
- optional/required upstreams, environment/header injection, and bounded
  upstream call timeouts.

Not implemented yet:

- skills/memory/artifact transport;
- ChatGPT setup widgets and diff UI;
- repository URL cloning and GitHub branch/PR/commit target selection;
- explicit project-switch/scratch/resume UX compatible with upstream Codexify;
- automatic download/update of `tunnel-client-runtime`;
- self-update.

The MCP core has been exercised with both raw MCP requests and the official Go
MCP client. End-to-end Windows SCM smoke tests verify the user-context lifecycle
and the multi-project/aggregation path:

```text
Windows SCM
  -> SYSTEM supervisor
       |-> tunnel runtime
       |-> <domain>\<interactive-user> worker
             -> MCP server
             -> exec_command whoami == <domain>\<interactive-user>
             -> long-running shell child

worker kill  -> worker restarts as the same interactive user
tunnel kill  -> tunnel restarts and re-authenticates to MCP
service stop -> worker + tunnel + long-running shell child are all gone

conversation A -> source checkout
conversation B -> same project -> managed worktree (auto mode)
catalog upstream -> search echo tool -> call echo tool -> bridged result
```

## Build

```powershell
go test ./...
go build -o bin\codexify-go.exe .\cmd\codexify-go
```

Go 1.26+ is currently used for development.

## Configuration

Copy `config.example.json` to an ignored local file:

```powershell
Copy-Item config.example.json config.local.json
```

Set:

- `mcp.workspaceRoot` to the directory this instance is allowed to access. In
  multi-project mode this is the **access root**, not an individual project;
- `tunnel.executable` to OpenAI's official `tunnel-client-runtime.exe`;
- `tunnel.tunnelId`;
- `tunnel.apiKeyRef` to an `env:NAME` or `file:C:\...` reference;
- `tunnel.mcpServerUrl` to the local MCP endpoint.

`tunnel.mcpServerUrl` must be an explicit loopback HTTP URL such as
`http://127.0.0.1:3300/mcp/tunnel_<id>`. When `mcp.authEnabled` is true (the
default), `codexify-go` generates a random bearer token at process start and
passes it to the tunnel child through an environment reference. It is never
written to `config.local.json`.

Do not put the OpenAI tunnel API key itself in Git.

### Multi-project mode

Enable durable conversation-scoped project selection with:

```json
{
  "mcp": {
    "workspaceRoot": "<absolute-or-env-expanded-access-root>",
    "multiProject": true,
    "projectScanDepth": 2,
    "worktrees": { "mode": "auto" }
  }
}
```

`list_projects` returns relative selectors beneath the access root.
`set_project_root` accepts one of those selectors. Bindings survive MCP/service
restarts and are immutable for that ChatGPT conversation. The raw
`openai/session` value is hashed before it is used as a binding key or filename.

When `worktrees.mode` is `auto`, the first conversation uses the source checkout
and a later conversation selecting the same Git project gets an isolated managed
worktree. `always` always isolates Git projects; `never` always uses the source
checkout unless an explicit worktree request is made, in which case selection
fails rather than silently ignoring the request.

### Upstream MCP aggregation

Upstreams are configured under `mcp.upstreams`. Example:

```json
{
  "name": "source-name",
  "transport": "stdio",
  "mode": "catalog",
  "command": "example-mcp-server",
  "args": [],
  "required": false,
  "timeout": "15s"
}
```

For Streamable HTTP use `"transport": "streamable_http"`, `"url": "..."`,
and optional `headers`. `catalog` keeps the upstream tool set private behind the
four `mcp_*` discovery/call tools; `direct` exposes tools as
`<source>__<original-tool-name>`.

Validate configuration without starting the tunnel:

```powershell
.\bin\codexify-go.exe doctor --config .\config.local.json
```

Run in the foreground:

```powershell
.\bin\codexify-go.exe run --config .\config.local.json
```

## Windows service

Service installation modifies SCM and therefore normally requires an elevated
terminal:

```powershell
.\bin\codexify-go.exe service install --config .\config.local.json
.\bin\codexify-go.exe service start   --config .\config.local.json
.\bin\codexify-go.exe service status  --config .\config.local.json
```

Removal:

```powershell
.\bin\codexify-go.exe service stop   --config .\config.local.json
.\bin\codexify-go.exe service remove --config .\config.local.json
```

The service is deliberately named `CodexifyGo` by default so it does not touch
or conflict with an installed Rust Codexify service.

## Windows identity model

The durable SCM service runs as `LocalSystem`, but developer-facing MCP tools do
not. The service discovers an active interactive WTS session, obtains that
session's user token, and starts `codexify-go worker run` with
`CreateProcessAsUser`. The worker receives the ephemeral MCP bearer through its
user environment and owns the MCP HTTP server, filesystem tools, Git and exec
sessions.

If no interactive user is logged in, the Windows service remains alive and the
worker supervisor retries with bounded backoff until an active session appears.

This removes the main LocalSystem limitation from Phase 2. Multi-project
bindings, managed worktrees, and upstream MCP aggregation also run inside this
user context, so Git credentials and user-scoped MCP commands retain the
interactive user's environment. Remaining gaps are primarily higher-level
Codexify product features: cloning/target selection, skills/memory,
artifact transport, and ChatGPT-specific setup/diff UI.

See [ARCHITECTURE.md](ARCHITECTURE.md).
