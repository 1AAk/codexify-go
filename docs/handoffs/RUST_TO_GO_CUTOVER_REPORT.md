# Rust to Go Cutover Report

## Run identity

- Started: 2026-09-29
- Host: Ubuntu 26.04.1 LTS, Linux x86_64
- Go toolchain: go1.26.5 linux/amd64
- Go source commit under test: `9cf3514f2f3ea9f529e6ff45c95691865db3f372`
- Go version: `0.8.2-dev`
- Rust baseline: Codexify `1.6.6`
- Managed tunnel runtime: `v0.0.12`
- Connector: the existing real ChatGPT connector, switched in place from Rust to Go
- Rust rollback: private local snapshot of the Rust binary, config, and systemd user unit created before cutover; no credentials are committed here

## Current status

Cutover is **in progress**. Platform parity is not being reimplemented during this run unless a cutover test exposes a concrete defect.

The real connector is currently served by Codexify Go. The Rust user service is stopped. This conversation continued through the transport switch and successfully executed Go MCP calls after the Rust service became inactive.

## Phase status

| Phase | Status | Evidence / notes |
| --- | --- | --- |
| 0 - Baseline and rollback | PASS | Rust 1.6.6 was healthy before cutover. Its binary was confirmed non-Go and identified itself as `Codexify MCP bridge (Rust)`. Private binary/config/unit rollback snapshot created before stopping it. |
| 1 - Go connector cutover | PASS with defects | Go 0.8.2-dev built from `9cf3514`, installed a separate systemd user unit and separate managed tunnel runtime, then took over the same real tunnel identity after Rust stopped. Current Go service and tunnel are healthy enough to serve this conversation. |
| 2 - Cold boot | PENDING | Requires a real reboot while Go remains the active implementation. |
| 3 - ChatGPT reconnect | PARTIAL | Hot transport replacement recovered automatically. Explicit disconnect/reconnect remains. |
| 4 - Conversation persistence | PARTIAL / DEFECT | The current conversation survived transport replacement, but the Rust project binding was not imported into Go and had to be selected again once. |
| 5 - Multi-project | PENDING | |
| 6 - Worktree lifecycle | PARTIAL | The Rust side had this conversation in a managed worktree. After Go cutover and rebinding, Go selected the source project rather than restoring the Rust worktree. Full Go create/switch/resume test remains. |
| 7 - Real Git delivery | PASS | This report and handoff update were committed and pushed to the user's fork through Go-only `exec_command` using the installed service's Git/SSH context. The final rebased delivery commit is `4602610`. |
| 8 - Long exec/stdin | PASS after schema refresh | After refreshing the connector schema, a long-running command returned a string session id and `write_stdin` accepted that id, delivered `go-only-stdin`, and observed normal completion. |
| 9 - Tunnel recovery | PASS | The Go-managed tunnel process was killed unexpectedly. The in-flight tool call disconnected/timed out as expected; the supervisor created a new tunnel process and the same connector resumed serving calls without Rust or manual repair. |
| 10 - Service recovery | PASS | The Go service was killed with SIGKILL. systemd restarted it with a new PID and incremented `NRestarts`; the tunnel and this conversation recovered while Rust stayed inactive. |
| 11 - Self-update | PENDING | |
| 12 - Bindings/state | PARTIAL | Cross-implementation Rust -> Go binding migration was absent at initial cutover, but after rebinding, the native Go conversation/project binding survived a Go service crash/restart. Reboot persistence remains. |
| 13 - Plugin skills | PASS | Go discovered the installed plugin registry and successfully read a real plugin skill. With the gateway enabled it also generated and discovered the `cutover_gateway` skill. |
| 14 - Upstream MCP | PARTIAL | A separately supervised standalone Streamable HTTP MCP server was independently probed, then connected as a required Go gateway. Both services remain active and Go generated the gateway skill. Calling the dynamically added gateway tool still requires a ChatGPT connector Refresh. |
| 15 - Attachments | PARTIAL | Real egress succeeded through `export_host_file` with a durable artifact reference and SHA-256. Real attachment ingress remains. |
| 16 - Rust dependency elimination | PARTIAL | Rust service is currently inactive and the active Go service owns its own binary, config, credential copy, and managed tunnel runtime. Rust files remain available only for rollback. Reboot and representative workflow suite remain. |

## Live process evidence

Immediately after cutover:

- Rust user service: `inactive`
- Go user service: `active`
- Go service executable: private cutover installation of the binary built from `9cf3514`
- Go service PID observed: `55004`
- Go-managed tunnel PID observed: `55035`
- tunnel runtime: Go-owned copy of `v0.0.12`
- MCP endpoint: loopback port `3300`
- the same ChatGPT conversation successfully invoked Go MCP after the switch

PIDs are observations for this run only and are expected to change after recovery tests.

## Defects found

### CUTOVER-001 - Rust config is not directly consumable by Go

**Severity:** P2 for the current controlled cutover; would become P1 if an in-place migration is expected to be automatic.

Rust 1.6.6 uses the current Rust configuration shape including `openaiTunnel`. Go 0.8.2-dev expects its own `tunnel` configuration shape and requires an explicit loopback `mcpServerUrl`.

For this run a separate Go config was created instead of mutating the Rust config. No infrastructure code was added because the cutover can proceed safely with an explicit migration step.

### CUTOVER-002 - Existing conversation/project binding is not migrated Rust -> Go

**Severity:** P1 candidate until expected migration semantics are decided.

After Go took over the connector, the existing conversation remained connected but Go returned that no project was selected. Selecting `codexify-go` again restored project access.

This is important for the final acceptance requirement that an existing conversation continues correctly after Rust becomes unavailable.

### CUTOVER-003 - Hot cutover exposes connector tool-schema drift

**Severity:** P1 candidate until explicit reconnect/schema refresh is tested.

The ChatGPT side initially retained tool definitions from the Rust connector. Two concrete mismatches were observed after Go takeover:

- an Rust-era optional `exec_command` argument was rejected by Go;
- `list_directory`, present in the Rust-era tool surface, returned `unknown tool` from Go.
- `git_commit`, present in the Rust-era tool surface, returned `unknown tool` from Go during the first real delivery attempt.

The Go `apply_patch` schema also differed from the Rust-era invocation shape. This may be resolved by an explicit connector schema refresh/reconnect, so no code change should be made until Phase 3/4 distinguishes stale client schema from a real Go parity gap.

The initial Phase 8 stdin check made this a concrete blocker before connector refresh: Go `exec_command` returned a string session id, while the stale ChatGPT-loaded `write_stdin` schema validated `session_id` as an integer before the call could reach Go.

After the user refreshed the connector tools, the Go schema loaded correctly and the same long-exec/stdin flow passed. This reclassifies the session-id mismatch as stale client schema during in-place implementation replacement rather than a Go exec/stdin runtime defect.

### CUTOVER-004 - Required upstream failure can cause an unbounded service restart loop

**Severity:** P2 operational robustness issue; evaluate before v1.0.

Two deliberately required upstream configurations were unavailable during startup. Go correctly failed startup because the upstream was marked required, but `Restart=on-failure` then retried indefinitely. The observed restart counter exceeded 100 attempts.

The first upstream, ChatGPT's bundled `node_repl`, was not a valid standalone test in the supplied environment. The second initial HTTP attempt also became unavailable because its temporary server was a child of the connector exec session. Neither is evidence that the Go gateway implementation itself is broken.

The corrected gateway test runs the standalone MCP server under a separate systemd user transient service. It was independently probed successfully before Codexify Go was restarted, and Codexify Go then connected to it successfully.

## Recovery evidence

### Tunnel crash recovery

The original Go-managed tunnel PID observed after cutover was `55035`. It was terminated unexpectedly during the live connector session. The active tool call lost transport and timed out, then the Go supervisor started a replacement tunnel process (PID `55619` in this run). A subsequent MCP command succeeded through the same ChatGPT connector while the Rust service remained inactive.

This satisfies the core Phase 9 recovery behavior. PID values are evidence for this run only.

## Rollback

The rollback remains intentionally local and private. The pre-cutover snapshot contains the Rust 1.6.6 executable, its configuration, and its systemd user unit. To roll back operationally, stop the Go cutover service and start the preserved Rust `codexify.service`; use the snapshot only if the installed Rust files themselves have been changed.

Do not commit the snapshot, tunnel credential, connector token, or machine-specific secret material.

## Next gates

1. Refresh/reconnect the connector and rerun long-running exec/stdin/cancellation with the Go schema.
2. Test Go service crash recovery.
3. Test native Go binding persistence across Go service restart.
4. Re-evaluate the missing/stale tool-surface differences after connector schema refresh.
5. Exercise multi-project and managed worktree create/switch/resume.
6. Exercise real plugin skills, upstream MCP, and attachment ingress/egress.
7. Exercise self-update.
8. Reboot with Rust still inactive and verify automatic Go recovery.
9. Continue several normal sessions with Rust unavailable before declaring v1.0 replacement.
