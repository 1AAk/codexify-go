# Unix Release Archives Design

## Decision

Codexify Go release archives use platform-native conventions:

- Windows: `.zip`
- macOS (Darwin): `.tar.gz`
- Linux: `.tar.gz`

The naming pattern remains `codexify-go-v<version>-<os>-<arch>.<ext>`.

## Compatibility

Published releases are immutable. Existing `v0.8.1` ZIP assets remain valid and are not rewritten. Because older Unix updaters query only `releases/latest` and request ZIP, Darwin/Linux continue to publish a ZIP compatibility asset alongside the preferred tar.gz artifact. New updaters select tar.gz; compatibility ZIPs prevent stale clients from losing the self-update path after later releases become latest.

## Self-updater

The updater keeps ZIP extraction for Windows and adds gzip-compressed tar extraction for Darwin/Linux. Extraction must:

- locate exactly the expected binary name;
- reject directories, links, devices, and other non-regular tar entries;
- enforce the existing binary-size bound;
- reject archives without the expected binary;
- preserve the updater's SHA-256 verification before extraction.

The staged binary is still written by Codexify Go with executable permissions, so archive mode bits are not trusted as the security boundary.

## Release workflow

The GitHub Release workflow packages Windows with `zip` and Darwin/Linux with `tar -czf`, plus a ZIP compatibility asset for each Unix target. `checksums.txt` contains basenames only and covers every published archive. The publish step uploads both `*.zip` and `*.tar.gz`.

## Documentation

README and architecture documentation describe the mixed archive contract and make clear that `v0.8.1` is the historical all-ZIP release while subsequent releases use native Unix tarballs.

## Verification

Tests cover archive-name selection, ZIP extraction, tar.gz extraction, rejection of unsafe tar entries, and the workflow asset contract. Final verification includes the full Go suite, race-sensitive packages, vet, native build, and all six cross-build targets.
