# Native Unix Release Archives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish Windows releases as ZIP and macOS/Linux releases as tar.gz while keeping self-update secure and compatible.

**Architecture:** Archive naming is selected from GOOS in the self-update release contract. Extraction dispatches by archive suffix: existing ZIP logic remains for Windows, while a bounded gzip+tar reader handles Unix archives. The release workflow publishes tar.gz as the preferred Unix format plus ZIP compatibility assets so already-published Unix updaters that inspect only `releases/latest` retain a valid update path.

**Tech Stack:** Go standard library (`archive/zip`, `archive/tar`, `compress/gzip`), GitHub Actions, Go tests.

---

### Task 1: Lock the archive contract with failing tests

**Files:**
- Modify: `internal/selfupdate/selfupdate_test.go`

- [ ] Add tests asserting Windows archive names end in `.zip`, Darwin/Linux release naming resolves to `.tar.gz`, and tar.gz extraction returns only the expected regular binary.
- [ ] Add a malicious tar-entry test that rejects a symlink/non-regular entry for the expected binary.
- [ ] Run `go test ./internal/selfupdate -count=1` and verify the new tar.gz tests fail because tar extraction/Unix naming is not implemented.

### Task 2: Implement dual archive extraction

**Files:**
- Modify: `internal/selfupdate/selfupdate.go`

- [ ] Add `archive/tar` and `compress/gzip`.
- [ ] Change release naming so Windows returns `.zip` and Darwin/Linux return `.tar.gz`.
- [ ] Dispatch extraction by archive name/format and implement bounded tar.gz extraction accepting only a regular file with the exact expected binary name.
- [ ] Run `go test ./internal/selfupdate -count=1` and verify all tests pass.

### Task 3: Update release workflow contract

**Files:**
- Modify: `.github/workflows/release.yml`
- Test: `internal/selfupdate/selfupdate_test.go`

- [ ] Extend the workflow-contract test to require conditional ZIP/tar.gz packaging and both publish globs.
- [ ] Run the contract test and verify it fails against the all-ZIP workflow.
- [ ] Update the build step to package Windows with ZIP and Darwin/Linux with tar.gz; generate basename-only SHA-256 sidecars for either format.
- [ ] Also package Darwin/Linux ZIP compatibility assets so older self-updaters that request ZIP from `releases/latest` remain functional.
- [ ] Update publish aggregation to consume both sidecar types and upload `*.zip` plus `*.tar.gz`.
- [ ] Re-run the contract test and verify it passes.

### Task 4: Document the public contract

**Files:**
- Modify: `README.md`
- Modify: `ARCHITECTURE.md`

- [ ] Replace the all-ZIP six-asset example with ZIP for Windows and tar.gz for Darwin/Linux.
- [ ] Document that v0.8.1 remains an immutable all-ZIP historical release and future releases use the mixed contract.

### Task 5: Release-gate verification

- [ ] Run `go test ./...`.
- [ ] Run race tests for self-update and security/process-sensitive packages.
- [ ] Run `go vet ./...`.
- [ ] Build the native binary and all six GOOS/GOARCH release targets.
- [ ] Run `git diff --check` and privacy scans.
- [ ] Review the aggregate project diff, commit the implementation, and push `main`.
