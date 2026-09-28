# codexify-go

Experimental Go implementation of the Codexify runtime architecture, starting
with the part that most benefits from native Windows integration: service
lifecycle and supervision of OpenAI's official Secure MCP Tunnel runtime.

The project is inspired by
[devnoname120/codexify](https://github.com/devnoname120/codexify), which is MIT
licensed. This repository is an independent implementation, not a line-by-line
port and not yet a drop-in replacement.

## Phase 1

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
- unit and Windows process integration tests.

Not implemented yet:

- Streamable HTTP MCP server;
- Codex-compatible filesystem/Git/exec tools;
- MCP server aggregation;
- conversation-to-workspace binding;
- managed Git worktrees;
- skills/memory/artifact transport;
- ChatGPT setup widgets and diff UI;
- automatic download/update of `tunnel-client-runtime`;
- self-update.

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

- `tunnel.executable` to OpenAI's official `tunnel-client-runtime.exe`;
- `tunnel.tunnelId`;
- `tunnel.apiKeyRef` to an `env:NAME` or `file:C:\...` reference;
- `tunnel.mcpServerUrl` to the local MCP endpoint.

Do not put the OpenAI tunnel API key itself in Git.

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

## Service-account boundary

SCM services run outside the logged-in desktop session. Phase 1 only supervises
the tunnel and is compatible with this model. A future full MCP implementation
must not blindly move all developer operations into LocalSystem: Git credential
manager state, SSH agents, DPAPI secrets and other per-user resources belong to
the user session.

The planned architecture therefore separates a system-level lifecycle
supervisor from a user-context MCP worker where user identity is required.

See [ARCHITECTURE.md](ARCHITECTURE.md).
