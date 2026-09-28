# Architecture

## Objective

Keep the useful external behavior of Codexify while making Windows lifecycle a
first-class subsystem rather than adapting a Unix daemon model through Task
Scheduler and PowerShell.

The current upstream Rust implementation intentionally uses a per-user
Scheduled Task on Windows. `codexify-go` starts from a different boundary:
Windows SCM owns the durable supervisor, and child runtimes are explicitly
monitored.

## Current topology (Phase 1 + Phase 2)

```text
Windows SCM
    |
    v
codexify-go.exe service run
    |
    +-- structured logger
    |
    +-- Streamable HTTP MCP server
    |     |
    |     +-- generated internal bearer auth
    |     +-- workspace confinement
    |     +-- filesystem/search tools
    |     +-- exec session manager
    |     +-- Git tools
    |
    +-- supervisor
          |
          +-- start tunnel-client-runtime.exe
          +-- monitor process exit
          +-- read health.url
          +-- GET /readyz
          +-- restart with bounded exponential backoff
          +-- terminate process tree on shutdown
```

There are two independent recovery layers:

1. **SCM recovery** restarts `codexify-go.exe` if the supervisor itself dies.
2. **Internal supervision** restarts only `tunnel-client-runtime.exe` if the
   child exits or becomes unhealthy.

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
When authentication is enabled, the parent generates a random bearer on every
start, injects it into the tunnel process environment, and configures the
tunnel's local `Authorization` header through an `env:` reference. The bearer
does not live in the JSON configuration.

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

## Windows process model

Child processes are created with `CREATE_NO_WINDOW` and
`CREATE_NEW_PROCESS_GROUP`. This avoids terminal popups.

On stop, the supervisor first asks Windows to terminate the process tree without
force. Some console-less children cannot be terminated this way; Windows
returns an error in that case. The supervisor then uses forced process-tree
termination rather than leaving an orphan.

Longer term this can be strengthened with a Windows Job Object owned directly
by the supervisor.

## Identity boundary

Running the full developer agent as LocalSystem would be convenient for
lifecycle but incorrect for many developer workflows. User-scoped resources
include:

- Git Credential Manager / DPAPI state;
- SSH agents and keys;
- user environment;
- mapped drives;
- profile-specific CLI configuration.

Planned full architecture:

```text
Windows SCM supervisor (system context)
    |
    +-- tunnel runtime
    |
    +-- user-session worker
          |
          +-- MCP HTTP server
          +-- filesystem tools
          +-- exec sessions
          +-- Git
          +-- upstream MCP bridges
          +-- project/worktree state
```

The exact authenticated IPC and user-session launch mechanism is intentionally
deferred until the MCP core is implemented and tested.

## Roadmap

### Phase 3: user-context worker and workspace compatibility

- authenticated supervisor <-> user-worker IPC;
- launch developer tools under the logged-in user rather than LocalSystem;
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
