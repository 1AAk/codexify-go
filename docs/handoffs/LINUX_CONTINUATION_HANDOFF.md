# Linux Continuation Handoff

## Goal

Finish Linux production/runtime parity for Codexify Go on a real Linux host.

This is a continuation handoff, not the final Rust -> Go cutover. Final replacement testing is defined separately in `docs/handoffs/RUST_TO_GO_CUTOVER_HANDOFF.md`.

Linux is complete only after the native service lifecycle, managed tunnel, MCP workflow, Git/SSH context, crash recovery, reboot/login recovery, and self-update are validated on a real Linux system.

## Repository baseline

Repository: `git@github.com:benice2me11/codexify-go.git`

At handoff creation:

- branch: `main`
- minimum baseline: `e01bb6b7768740b53c026f29443cd6a0c04e05a3`
- version: `0.8.2-dev`
- stable release: `v0.8.1`
- Windows amd64: production-validated
- macOS arm64: production-validated
- Linux amd64/arm64: CI/cross-build validated only

Start by pulling latest `main`. Do not reset newer work back to the baseline SHA.

```bash
git pull --ff-only
git status --short --branch
git log --oneline -8
git merge-base --is-ancestor e01bb6b7768740b53c026f29443cd6a0c04e05a3 HEAD
grep 'const Version' internal/buildinfo/buildinfo.go

uname -a
uname -m
cat /etc/os-release
systemctl --version
systemctl --user status
go version
```

Do not assume distro, architecture, desktop/headless mode, VM/container/WSL, or package manager before inspecting the host.

## What already works on Linux

Linux CI already exercises the platform-neutral core:

- MCP server and tools;
- workspace confinement;
- multi-project bindings;
- worktrees;
- Git operations;
- long exec sessions/stdin;
- POSIX process groups and forced cleanup;
- skills/plugins/project docs/memory;
- upstream MCP catalog/gateway;
- artifacts;
- diff checkpoints;
- Markdown chat;
- agent tickets;
- connector schema state;
- self-update preparation;
- linux/amd64 and linux/arm64 builds.

Managed OpenAI tunnel runtime assets and pinned hashes already exist for Linux amd64 and arm64.

Release contract is already prepared:

- Linux tar.gz = preferred current Unix artifact;
- Linux ZIP = compatibility asset for older self-updaters;
- new Unix updater selects tar.gz.

## Main missing code

`internal/service/service_other.go` still handles Linux through an unsupported stub:

```go
//go:build !windows && !darwin
```

There is no `internal/service/service_linux.go`.

This is the main Linux implementation gap.

Linux should **not** copy the Windows user-worker architecture. `app.Run` uses the separate user worker only on Windows. A per-user systemd service should run `runInProcess` directly as the logged-in Linux user.

The existing non-Windows self-update path already replaces the binary synchronously. Once Linux `service.Query/Stop/Start` work, the CLI can stop the service, replace the binary, and start it again without a Linux update-worker.

## Required service architecture

Use a **systemd user service**, not a root/system daemon.

Normal service management must not require sudo.

Expected unit directory:

```text
$XDG_CONFIG_HOME/systemd/user/
```

with fallback to:

```text
~/.config/systemd/user/
```

Do not hardcode `/home/<user>`.

Before implementing, verify the host has a usable user manager:

```bash
systemctl --user status
systemctl --user show-environment
echo "$XDG_RUNTIME_DIR"
systemctl --user list-units --type=target
```

If `systemctl --user` is unavailable, stop and classify the environment. Do not silently switch to a root service.

### Autostart semantics

Use:

```ini
[Install]
WantedBy=default.target
```

Do not automatically enable user lingering. `loginctl enable-linger` changes account/system behavior and may require admin authorization. Test it only if the user explicitly wants pre-login or post-logout operation.

Do not blindly depend on system-level `network-online.target` from the user unit. Codexify/tunnel already has retry/health/backoff behavior and should recover from temporary network unavailability.

### Target unit shape

Conceptually:

```ini
[Unit]
Description=Codexify Go

[Service]
Type=exec
ExecStart=<absolute-codexify-go> service run --config <absolute-config>
Restart=on-failure
RestartSec=5s
KillMode=control-group
TimeoutStopSec=<bounded value compatible with supervisor.shutdownTimeout>
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
```

Requirements:

- absolute executable and config paths;
- no shell wrapper;
- no secret in the unit;
- no `User=` in a per-user unit;
- `Restart=on-failure`;
- cgroup-wide cleanup;
- enabled for `default.target`.

## Secrets/environment

Prefer `tunnel.apiKeyRef` using a private file reference for Linux validation, with the key file mode `0600`.

Do not assume interactive shell variables are inherited by the systemd user manager after reboot. If env-based credentials are tested, verify persistence explicitly. Never solve this by embedding a raw secret in the unit.

## Implementation tasks

### 1. Establish clean native baseline

Run:

```bash
go mod download
go test ./...
go vet ./...
go build -trimpath -o bin/codexify-go ./cmd/codexify-go
./bin/codexify-go version
```

Keep both Linux cross-builds green.

### 2. Implement Linux service backend

Expected changes:

- add `internal/service/service_linux.go`
- add `internal/service/service_linux_test.go`
- change fallback build tag in `service_other.go` to exclude Linux

Implement existing API without changing CLI shape:

- `Install`
- `Start`
- `Stop`
- `Restart`
- `Remove`
- `Query`
- `Run`
- `ExecutablePath`
- `IsRunning`
- `StateString`

`Run` should use normal signal cancellation and `app.Run(..., false)`, like the macOS in-process service model.

### 3. Unit naming and escaping

Never use arbitrary `service.name` directly as a path/unit identifier.

Add deterministic bounded normalization and tests.

Generated `ExecStart` must correctly represent absolute paths containing spaces and percent characters without falling back to `/bin/sh -c`.

### 4. Install/remove transaction

Install should:

1. resolve executable/config absolute paths;
2. resolve safe user-unit directory;
3. create it;
4. refuse unsafe/unrelated overwrite;
5. atomically write the unit;
6. daemon-reload;
7. enable for `default.target`;
8. leave actual start to explicit `service start`.

Remove should:

1. stop successfully or confirm stopped;
2. disable;
3. remove only expected unit;
4. daemon-reload;
5. never delete the unit after an unhandled stop failure.

### 5. Query/lifecycle

Use machine-readable `systemctl --user show` / exit codes rather than parsing colored human status output.

Distinguish:

- not installed;
- installed/stopped;
- installed/running.

Test install/start/stop/restart/remove/status idempotency and failure behavior.

### 6. Unit tests

Cover at least:

- unit-name normalization;
- unit rendering;
- absolute ExecStart/config;
- `WantedBy=default.target`;
- `Restart=on-failure`;
- `KillMode=control-group`;
- no raw secrets;
- path escaping;
- systemctl state parsing;
- install/remove path confinement.

Unit tests must not mutate the developer's real systemd user manager.

## Real Linux validation

Unit tests are not enough.

### A. Native lifecycle smoke

Use a dedicated smoke service/config.

Required flow:

```text
doctor
-> status: not installed
-> install
-> inspect/verify generated unit
-> start
-> status: running
-> MCP health
-> tunnel health
-> restart
-> status: running
-> stop
-> status: stopped
-> start
-> remove
-> status: not installed
```

After cleanup verify no unit, enable symlink, service process, tunnel child, or smoke state remains.

### B. Crash recovery

With the real service running:

1. SIGKILL tunnel child;
2. confirm supervisor creates a different healthy tunnel PID;
3. crash/kill Codexify service process;
4. confirm systemd restarts it;
5. confirm MCP/tunnel health recovers;
6. confirm no orphan descendants.

Useful evidence:

```bash
systemctl --user show <unit> -p MainPID -p ActiveState -p SubState -p Result
journalctl --user-unit <unit> --no-pager
```

### C. Login/reboot recovery

Mandatory for production validation:

1. service enabled;
2. reboot or fully restart user session/manager;
3. log in normally;
4. do not manually start Codexify;
5. verify service active;
6. verify tunnel healthy;
7. verify ChatGPT connector reconnects.

Test linger separately only if explicitly desired.

### D. Managed tunnel runtime

On real Linux:

```bash
codexify-go tunnel install --config <config>
codexify-go tunnel status --config <config>
codexify-go tunnel verify --config <config>
file <managed-tunnel-binary>
```

Use the real managed runtime in final service smoke, not only a fake tunnel.

### E. Real MCP workflow

Through the installed Linux service/tunnel verify:

- connector initialize;
- project selection;
- agent brief/environment;
- read/write/apply_patch;
- long exec + stdin;
- show_diff;
- Git status;
- plugin skill discovery;
- upstream MCP gateway if configured;
- attachment import/export if available.

### F. Git/SSH from service context

This is mandatory.

Through Codexify Go running under systemd --user:

```text
read/edit
-> tests
-> diff/status
-> commit
-> push
```

Do not declare Linux parity if push works only from an interactive terminal but not from service-mode Codexify.

### G. Linux self-update

Use a real published test/stable release when appropriate.

Verify:

1. update check;
2. Linux tar.gz selected as preferred archive;
3. checksum verification;
4. tar extraction/staging;
5. service stop;
6. binary replacement;
7. service start;
8. version changed;
9. connector recovered;
10. state persisted.

Also preserve ZIP compatibility assets in release publishing for old Unix updaters.

## Config/documentation cleanup

During Linux implementation also fix the Windows-biased public example.

Current `config.example.json` should not remain Windows-only once Linux is supported.

Update README/Architecture to state Linux is production-validated only **after** live validation passes.

Do not mark compile-only architectures as runtime-validated.

## Verification gate before commit/push

Run:

```bash
gofmt -w .
go test ./...
go test -race ./internal/service ./internal/supervisor ./internal/execsession ./internal/selfupdate ./internal/workspace ./internal/projects
go vet ./...
go build -trimpath -o bin/codexify-go ./cmd/codexify-go
git diff --check
```

Cross-build:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/codexify-go
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./cmd/codexify-go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/codexify-go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/codexify-go
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/codexify-go
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/codexify-go
```

Run privacy scan before commit. Do not commit hostnames, usernames, home paths, credentials, tunnel keys, or machine-specific absolute paths.

After push, require a fully green GitHub Actions matrix.

## Linux acceptance checklist

Linux is production-validated only when all are true:

- [ ] native `service_linux.go` exists;
- [ ] no Linux fallback to unsupported service stub;
- [ ] systemd user install/start/stop/restart/remove/status work;
- [ ] unit enabled under `default.target`;
- [ ] normal lifecycle requires no root;
- [ ] no automatic linger side effect;
- [ ] cold login/reboot recovery passes;
- [ ] service crash recovery passes;
- [ ] tunnel crash recovery passes;
- [ ] no orphan process tree;
- [ ] managed OpenAI tunnel runtime verified on real Linux;
- [ ] real MCP connector session works;
- [ ] multi-project/worktree operations work;
- [ ] long exec/stdin works;
- [ ] real Git commit/push works from service context;
- [ ] plugin skills work;
- [ ] upstream MCP works if configured;
- [ ] attachment ingress/egress works if available;
- [ ] Linux tar.gz self-update works end-to-end;
- [ ] state/bindings survive restart/reconnect;
- [ ] config example/docs are no longer Windows-biased;
- [ ] full local tests/race/vet/build pass;
- [ ] GitHub Actions matrix is green;
- [ ] no unresolved P0/P1 Linux defects.

## After Linux parity

Do not automatically release a new stable version unless the user asks.

Keep `main` on the current dev cycle, document the validated distro/kernel/systemd/architecture, and then proceed to the separate Rust -> Go cutover handoff.

Passing this handoff means Linux has joined Windows/macOS as a production-validated platform. It does **not** by itself prove that Rust Codexify can be removed; that final proof belongs to `RUST_TO_GO_CUTOVER_HANDOFF.md`.
