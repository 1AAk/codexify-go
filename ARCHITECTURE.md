# Architecture

## Objective

Keep the useful external behavior of Codexify while making Windows lifecycle a
first-class subsystem rather than adapting a Unix daemon model through Task
Scheduler and PowerShell.

The current upstream Rust implementation intentionally uses a per-user
Scheduled Task on Windows. `codexify-go` starts from a different boundary:
Windows SCM owns the durable supervisor, and child runtimes are explicitly
monitored.

## Current topology (Phase 1 + Phase 2 + Phase 3)

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
    |     +-- workspace confinement
    |     +-- filesystem/search tools
    |     +-- exec session manager
    |     +-- Git tools
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

- `get_environment`;
- `read_file`, `write_file`;
- `glob`, `grep`;
- `exec_command`, `write_stdin`;
- `git_status`, `git_diff`, `git_log`.

All path-taking tools resolve against one configured workspace root. Absolute
paths and `..` escapes are rejected, and symlink resolution is checked against
the canonical root before access.

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

The Phase 3 smoke test verifies both process ownership (`FOXOS\FoxOS_User`) and
tool identity (`exec_command whoami` returns `foxos\foxos_user`).

## Roadmap

### Phase 3: workspace compatibility (remaining)

- multi-project catalogue;
- conversation binding;
- safe project selection;
- worktree lifecycle;
- connector/MCP aggregation;
- skills and persistent memory.

### Phase 4: ChatGPT integration

- connector schema parity;
- artifact ingress/egress;
- setup/status UI;
- diff widget metadata;
- update/install UX.

## Upstream reference

Primary reference:

- `https://github.com/devnoname120/codexify`
- MIT License

In particular, upstream `src/service.rs` documents the current Windows
Scheduled Task implementation, service-level restart policy and process-tree
handling. This project uses that behavior as a compatibility reference while
implementing a different Windows lifecycle architecture.
