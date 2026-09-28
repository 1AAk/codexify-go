# Architecture

## Objective

Keep the useful external behavior of Codexify while making Windows lifecycle a
first-class subsystem rather than adapting a Unix daemon model through Task
Scheduler and PowerShell.

The current upstream Rust implementation intentionally uses a per-user
Scheduled Task on Windows. `codexify-go` starts from a different boundary:
Windows SCM owns the durable supervisor, and child runtimes are explicitly
monitored.

## Current topology (Phase 1 through Phase 5)

```text
Windows SCM
    |
    v
codexify-go.exe service run (LocalSystem)
    |
    +-- structured logger
    |
    +-- user-worker supervisor
    |     |
    |     +-- discover active WTS session
    |     +-- CreateProcessAsUser
    |     +-- kill-on-close Job Object
    |     |
    |     v
    |   codexify-go.exe worker run (interactive user)
    |     |
    |     +-- Streamable HTTP MCP server
    |     +-- generated internal bearer auth
    |     +-- project catalogue + conversation bindings
    |     +-- repository clone/target materialization
    |     +-- managed Git worktrees
    |     +-- switch / scratch / resume lifecycle
    |     +-- workspace confinement per active binding
    |     +-- filesystem/search tools
    |     +-- exec session manager
    |     +-- Git tools
    |     +-- memory + skills
    |     +-- artifact egress store
    |     +-- setup + diff MCP App resources
    |     +-- upstream MCP bridge
    |           +-- stdio / Streamable HTTP
    |           +-- catalog / direct exposure
    |           +-- opaque ResourceLink capabilities
    |
    +-- tunnel supervisor
          |
          +-- start tunnel-client-runtime.exe
          +-- monitor process exit
          +-- read health.url
          +-- GET /readyz
          +-- restart with bounded exponential backoff
          +-- terminate process tree on shutdown
```

There are three independent recovery layers:

1. **SCM recovery** restarts `codexify-go.exe` if the supervisor itself dies.
2. **Worker supervision** restarts only the user-context MCP worker if it exits
   or its `/health` endpoint becomes unhealthy.
3. **Tunnel supervision** restarts only `tunnel-client-runtime.exe` if the
   tunnel child exits or becomes unhealthy.

This prevents a tunnel failure from unnecessarily restarting the whole agent.

## Restart policy

Child tunnel:

- minimum delay: 2 seconds by default;
- exponential backoff to 60 seconds;
- after a stable runtime window, backoff resets;
- readiness checks begin after the tunnel startup grace period;
- readiness is sampled periodically;
- consecutive readiness failures trigger a child-only restart.

Windows service:

- first failure: restart after 5 seconds;
- second failure: restart after 15 seconds;
- subsequent configured failure: restart after 60 seconds;
- failure count resets after 24 hours without a failure.

## Tunnel contract

`codexify-go` does not reimplement OpenAI's tunnel protocol. It executes the
official `tunnel-client-runtime` with the same public command-line concepts used
by Codexify:

- tunnel id;
- API-key reference (`env:` or `file:`);
- local MCP server URL;
- optional MCP Authorization reference;
- startup wait timeout;
- loopback health listener;
- health URL file;
- JSON logging.

The tunnel health URL file contains a loopback base URL. The supervisor appends
`/readyz` and rejects non-loopback health URLs.

The local MCP endpoint is also restricted to an explicit loopback HTTP URL.
When authentication is enabled, the SYSTEM parent generates a random bearer on
every start. The same bearer is injected independently into the user worker and
the tunnel runtime through their environment blocks. The tunnel's local
`Authorization` header uses an `env:` reference, while the worker validates the
corresponding bearer. The secret never lives in the JSON configuration.

## MCP protocol

Phase 2 uses the official `github.com/modelcontextprotocol/go-sdk/mcp` server
rather than maintaining a private JSON-RPC implementation. The HTTP handler is
stateless, which lets the official SDK negotiate the current MCP revision while
remaining compatible with older Streamable HTTP clients.

Current tools:

- `list_projects`, `set_project_root`, `setup_ui_switch_project`,
  `setup_status`, `list_worktrees`;
- `get_environment`, `get_agent_brief`, `get_project_doc`;
- `read_file`, `write_file`, `apply_patch`;
- `glob`, `grep`;
- `exec_command`, `write_stdin`;
- `git_status`, `git_diff`, `show_diff`, `git_log`;
- `remember`, `recall`, `update_memory_note`, `forget_memory_note`;
- `skills_list`, `skills_read`;
- `export_host_file`, `import_host_file`.

When at least one upstream is configured in `catalog` mode, the worker also
registers `mcp_list_sources`, `mcp_search_tools`, `mcp_get_tool`, and
`mcp_call_tool`. Upstreams configured in `direct` mode are exposed with a
sanitized `<source>__<tool>` name.

All path-taking tools resolve against the active conversation workspace.
Absolute paths and `..` escapes are rejected, and symlink resolution is checked
against the canonical active root before access.

## Multi-project conversation binding

With `mcp.multiProject=false`, the configured `workspaceRoot` remains the active
workspace exactly as in earlier phases. With `multiProject=true`, it becomes an
access root containing selectable projects.

The ChatGPT tunnel forwards a stable conversation identifier in MCP request
metadata under `_meta["openai/session"]`. `codexify-go` hashes the raw value with
SHA-256 plus a domain separator and persists only that digest. One JSON binding
file stores the selected source project and active workspace. Re-selecting the
same project is idempotent; selecting a different project from the same
conversation is rejected.

Project discovery is bounded by `projectScanDepth`, skips common build/vendor
directories, detects common project markers, and can be supplemented with
explicit `mcp.projects` entries. `list_projects` returns relative selectors so
configuration and persisted metadata do not depend on a developer's literal
home-directory string.

Repository references add a second selection route. HTTPS/SSH references are
parsed into a normalized repository identity; credential-bearing URLs and local
filesystem transports are rejected. The resolver reuses a unique matching
checkout beneath the access root or clones into a private staging directory and
atomically publishes the checkout. GitHub HTTPS URLs may specify a branch,
pull-request head, or full commit. The exact target is fetched non-interactively
and converted into an isolated worktree whenever changing the source checkout
would otherwise be required.

Workspace switching never mutates a binding in place. The prior binding is
archived, selection becomes explicitly pending, and the next
`set_project_root` chooses a new project/scratch workspace. `resumePath` can only
rebind an exact active workspace already present in durable binding history.

## Managed worktrees

Worktree placement is controlled by `mcp.worktrees.mode`:

- `never`: use the source checkout;
- `always`: create a managed worktree for Git-backed selections;
- `auto`: use the source checkout until another active conversation already
  references the same source project, then isolate the later conversation.

Managed worktrees use `git worktree add` and a per-conversation branch under a
private worktree root. The selected project may be a subdirectory of a larger
Git repository; the same relative subdirectory is selected inside the managed
worktree. Binding persistence records both the source project and active
worktree so service/MCP reconnects reuse the same workspace.

## Upstream MCP aggregation

Aggregation runs inside the interactive-user worker. This is important for
stdio MCP servers that depend on user PATH, Git credentials, SSH agents, or
other per-user state.

Supported transports:

- stdio via the official Go MCP SDK `CommandTransport`;
- Streamable HTTP via `StreamableClientTransport`, with optional request
  headers.

Supported exposure modes:

- `catalog` (default): tools remain private and are discovered/called through
  four compact catalog tools;
- `direct`: each upstream tool is proxied into the main MCP catalog using a
  collision-safe sanitized name;
- `gateway`: one compact downstream tool routes to all upstream functions and a
  generated private `SKILL.md` exposes the exact function schemas progressively.

Optional upstream failures are reported and skipped; `required=true` makes an
upstream connection failure fail worker startup. Tool calls have a configured
timeout and bridge sessions are closed during worker shutdown.

Resource links returned by bridged tools are converted into opaque downstream
capabilities. The bridge stores only the upstream source/session and original
URI in private worker memory. `resources/read` on the opaque URI is forwarded
back upstream, result URIs are rewritten to the capability URI, and total
payload size is bounded by `artifactEgress.maxFileBytes`.

## Memory and skills

Project memory lives outside the repository in private user state. Its identity
is derived from the canonical active workspace path and the note set has a
bounded aggregate size. Create/update/delete semantics are deliberately strict
so an agent cannot silently overwrite a decision under an existing key.

Skill discovery searches the active repository/workspace and configured user
roots for `SKILL.md`. YAML frontmatter provides the small list-time catalogue;
the body and package resources are read only after a skill is chosen. Resource
resolution is package-confined and rejects absolute paths, `..`, and symlink
escapes. Client-visible skill paths are scope-relative instead of host-absolute.

## Artifact egress and MCP Apps

`export_host_file` resolves a workspace-relative regular file through the same
canonical path guard used by file tools. An opaque capability URI references
either an immutable private snapshot or, when configured, a bounded-TTL safe
source fallback. Snapshot count/total bytes and individual file size are
bounded, and no local filesystem path is embedded in the public capability.

Artifact ingress is a separate trust boundary. `import_host_file` accepts only
host-authorized HTTPS file references, applies an explicit allowed-host policy,
revalidates redirects and DNS results against loopback/private/link-local ranges,
uses request/idle/size/concurrency limits, and creates a new destination only.

Setup and diff surfaces are independent MCP App resources. Tool metadata points
to self-contained `text/html;profile=mcp-app` documents with no external CSP
dependencies. The setup app invokes server tools (`setup_status`, selection,
switch, scratch) and therefore does not duplicate authoritative workspace
state. The diff app renders `show_diff` result metadata.

## Agent brief and project instructions

`get_agent_brief` is the post-selection handoff boundary. It renders generic
coding workflow guidance, environment/workspace identity, saved memory and the
skill catalogue first, then appends project-owned instructions last. Project
documents are discovered from repository root to active workspace; each level
prefers `AGENTS.override.md` over `AGENTS.md` and all documents share one byte
budget. This ordering ensures repository-specific instructions override the
generic agent brief without allowing user-state memory to masquerade as project
instructions.

## Patch and diff checkpoint model

`apply_patch` preflights the complete patch before the first write: every source
and destination resolves through the canonical workspace guard, update contexts
must match, and duplicate targets are rejected. Add/delete/update/move actions
then execute sequentially with an explicit partial-failure report if an OS write
fails after preflight.

Before mutation tools change a Git-backed workspace, the diff manager ensures a
`project_open` checkpoint. Snapshots use a private temporary index and
`git write-tree`, so user staging is never changed. Persistent ChatGPT owners use
private Git refs for `project_open`/`last_diff`; transient transport owners use
in-memory checkpoint state that is discarded with the MCP session. `show_diff`
can inspect without advancing or atomically advance the private `last_diff`
cursor after a successful comparison.

## Hybrid MCP identity model

The HTTP endpoint routes current protocol/discovery requests through stateless
Streamable HTTP. Legacy stateful sessions receive a server `Mcp-Session-Id`.
When no stable `openai/session` metadata is present, that ID is domain-separated
and hashed into a non-persistent transport identity. Workspace and diff state are
removed when the server session closes; a reconnect therefore cannot silently
inherit the previous selection.

## Connector schema bookkeeping

The runtime publishes a compact connector marker built from the release version
and schema-changing feature suffixes (`workspace`, artifact ingress, gateway).
Tunnel-scoped and first-observed conversation markers are persisted privately
under the interactive user's state directory. `setup_status` compares those
markers and exposes `conversationStale`; this is UI/reload bookkeeping, not an
authentication mechanism.

## Windows process and identity model

Tunnel children are created with `CREATE_NO_WINDOW` and
`CREATE_NEW_PROCESS_GROUP`. This avoids terminal popups.

The MCP worker is different: the SCM process discovers an active WTS session,
obtains the logged-in user's token with `WTSQueryUserToken`, builds that user's
environment block, and launches the worker with `CreateProcessAsUser` in a
suspended state. Before resuming it, the parent assigns the worker to a Windows
Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`.

This ordering matters: shell commands and other descendants spawned immediately
by the worker are born inside the Job Object and cannot escape supervision. A
service stop or worker replacement therefore removes the complete worker
process tree.

On stop, the supervisor first asks Windows to terminate the process tree without
force. Some console-less children cannot be terminated this way; Windows
returns an error in that case. The supervisor then uses forced process-tree
termination rather than leaving an orphan.

## Identity boundary

The durable lifecycle layer remains `LocalSystem`, while developer operations
run in the active user's session. This preserves access to user-scoped resources
such as:

- Git Credential Manager / DPAPI state;
- SSH agents and keys;
- user environment;
- mapped drives;
- profile-specific CLI configuration.

The Phase 3 smoke test verifies both process ownership and tool identity against
the currently active interactive Windows user, without relying on a hard-coded
account name or profile path.

## Roadmap

### Remaining Codexify compatibility

- installed Codex/Claude plugin skill discovery and optional Claude skill roots;
- fuller upstream connector-schema discovery/reload migration history and public
  update-status checks;
- Markdown-chat / agent-ticket product surfaces;
- automatic tunnel-client acquisition/update and self-update UX.

## Upstream reference

Primary reference:

- `https://github.com/devnoname120/codexify`
- MIT License

In particular, upstream `src/service.rs` documents the current Windows
Scheduled Task implementation, service-level restart policy and process-tree
handling. This project uses that behavior as a compatibility reference while
implementing a different Windows lifecycle architecture.
