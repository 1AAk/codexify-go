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
  upstream call timeouts;
- safe HTTPS/SSH repository selection with local-checkout reuse and private
  cloning below the access root;
- exact GitHub HTTPS branch, pull-request, and full-commit target selection;
- explicit `setup_ui_switch_project`, persistent scratch workspaces, and
  validated `resumePath` continuation of previously saved workspaces;
- project-scoped durable memory through `remember`, `recall`,
  `update_memory_note`, and `forget_memory_note`;
- repo/user skill discovery through `skills_list` and package-confined
  `skills_read`, including `agents/openai.yaml` implicit-invocation policy;
- `export_host_file` with opaque capability URIs, immutable snapshots when they
  fit the configured budget, and bounded source-backed fallback references;
- opaque proxy resources for `ResourceLink` values returned by upstream MCP
  tools, with downstream `resources/read` forwarding and size limits;
- compact MCP App resources for workspace setup/status and `git_diff`, using
  standard `ui/resourceUri` and OpenAI output-template metadata;
- `get_agent_brief` / `get_project_doc` with environment, saved memory, skill
  catalogue, and `AGENTS.override.md` / `AGENTS.md` discovery from repository
  root to the active workspace; project instructions are rendered last;
- workspace-confined `apply_patch` using Codex patch grammar with full
  preflight/context validation before the first write;
- checkpointed `show_diff` with immutable `project_open` and incremental
  `last_diff` baselines, tracked/untracked/deleted state, bounded patch metadata,
  and a private diff cursor that does not modify the user's Git index;
- secure `import_host_file` for host-authorized ChatGPT/native file references:
  HTTPS-only downloads, host allowlist, redirect/DNS/private-IP checks, byte and
  timeout bounds, and no overwrite of existing workspace files;
- hybrid MCP transport: current `2026-07-28` clients use stateless requests,
  while legacy/stateful clients without `openai/session` receive an isolated
  transport-session workspace binding that is forgotten on disconnect;
- upstream MCP `gateway` mode: one compact gateway tool plus a generated
  `SKILL.md` containing the upstream function list and exact input schemas;
- tunnel-scoped connector schema markers surfaced by `setup_status`, including
  per-conversation stale-version detection for setup/reload UX.

Not implemented yet:

- plugin-contributed skills and optional Claude skill discovery;
- full upstream connector-schema discovery/reload migration history and bounded
  public release/update checks (the local schema marker/stale status is present);
- upstream Markdown-chat / agent-ticket product surfaces;
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

GitHub commit URL -> private clone -> fetched exact commit -> managed worktree
project -> remember/export -> switch -> scratch -> skill read -> resume project
setup MCP App resource -> readable with text/html;profile=mcp-app

get_agent_brief -> gateway skill + AGENTS.md at highest project priority
apply_patch -> show_diff(project_open) -> apply_patch -> show_diff(last_diff)
host file reference -> HTTPS ingress -> new workspace file
legacy stateful MCP session -> transient project binding -> disconnect -> forgotten
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
worktree. `always` isolates Git projects and `never` uses the source checkout.
An explicit `createWorktree=true/false` on `set_project_root` overrides the
configured mode for that selection.

`set_project_root.path` may also be a safe repository URL. GitHub HTTPS URLs
support repository roots plus `/tree/<branch>`, `/pull/<number>`, and
`/commit/<full-40-char-sha>`. Generic non-GitHub HTTPS/SSH URLs must end in
`.git`. Credential-bearing HTTPS URLs, `file://`, and other local/insecure
transport forms are rejected. Matching local checkouts are reused; otherwise a
clone is staged and published inside the configured private clone directory.
Targeted revisions never silently move an unrelated source checkout: when the
requested commit is not already checked out, an isolated worktree is required.

Workspace changes are explicit. `setup_ui_switch_project` archives the active
binding without deleting its files/worktree; `set_project_root` can then select
another project or `withoutProject=true` for persistent scratch. A later/new
conversation may pass an exact saved active workspace as `resumePath`; arbitrary
filesystem paths are not accepted as resumable state.

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

When an upstream tool returns a `ResourceLink`, `codexify-go` replaces its
original URI with an opaque `codexify-go://upstream-resource/...` capability.
Reading that capability proxies `resources/read` back to the originating MCP
server and enforces `artifactEgress.maxFileBytes`.

### Memory, skills, and file export

Memory is stored outside the repository and keyed by the active workspace. It
is intended for concise project decisions/facts rather than conversation logs;
the default aggregate note budget is 16 KiB.

Skills are progressively disclosed: `skills_list` reads only `SKILL.md`
frontmatter, and `skills_read` loads the selected body/resource. Public skill
descriptors use scope-relative package paths rather than absolute host paths.

`export_host_file` accepts an active-workspace-relative regular file and returns
an opaque downloadable MCP resource. By default files up to 100 MiB may be
exported, snapshots use a bounded durable store, and a non-snapshotted resource
may safely fall back to the latest source file for a bounded TTL. No local host
path appears in the resource URI or export receipt.

`import_host_file` is the inverse path for a host-authorized native file
reference. The tool advertises `openai/fileParams`, accepts the temporary
download URL/file metadata supplied by the host, validates every redirect and
resolved address, and writes only to a new workspace-relative destination.
Arbitrary local source paths and overwrite-by-import are not supported.

### Agent brief, patching, and diff checkpoints

After selecting or switching a workspace, call `get_agent_brief`. It combines
generic coding workflow guidance, current environment/workspace information,
saved project memory, the progressive skill catalogue, and repository
instructions. Project `AGENTS.override.md` / `AGENTS.md` content is appended
last so repository-specific instructions have the highest project-level
priority.

`apply_patch` implements the Codex patch grammar (`*** Begin Patch`, add/delete/
update/move actions and contextual hunks). Every path and update context is
validated before the first filesystem mutation; paths cannot escape the active
workspace.

`show_diff` captures the complete working-tree snapshot through a temporary Git
index (`GIT_INDEX_FILE` + `git add -A` + `git write-tree`), so untracked/deleted
files and mode changes are represented without touching the user's staging
area. `since=project_open` compares against the immutable selection checkpoint;
`since=last_diff` compares against the private incremental cursor. The bounded
patch is placed in component/widget metadata rather than model-visible
structured output.

### Generic MCP clients

ChatGPT conversations use their stable hashed `openai/session` identity and
persist bindings. Older/generic stateful MCP clients that lack that metadata
receive a transport-session identity instead. Such a binding is intentionally
transient: reconnecting creates a new unbound session, while any files already
written to the chosen workspace remain untouched.

### MCP Apps

The server exposes small self-contained setup and diff resources with MIME type
`text/html;profile=mcp-app`. The setup app calls the same server-side
`setup_status`, project-selection, scratch, and switch tools; it has no separate
workspace state. `show_diff` carries the diff-app/result metadata and remains
usable as a normal MCP tool in clients that ignore MCP Apps metadata.

`setup_status` also exposes a connector schema marker such as
`0.6.0-dev+workspace-v1+artifact-ingress-v1+gateway-v1`. A conversation can
echo the marker it currently holds; the server records the first observed
conversation marker privately and reports `conversationStale` when the active
server schema has changed.

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

This removes the main LocalSystem limitation from Phase 2. Repository cloning,
target fetches, multi-project bindings, managed worktrees, skills, memory,
artifact export, and upstream MCP aggregation all run inside this user context,
so Git Credential Manager, SSH agents, PATH, and other user-scoped state remain
available to developer workflows.

See [ARCHITECTURE.md](ARCHITECTURE.md).
