# Build the supervisor binary in CI (and Makefile)

## Overview

The nightly CI pipeline (`.github/workflows/nightly.yml`) builds, checksums,
and releases the `vsockd` binary per-arch, but not the new `supervisor` binary
(`./cmd/supervisor`, added in the enclave-log-delivery work). The `Makefile`
`build` target has the same gap — it builds only `vsockd`. This change adds
`supervisor` alongside `vsockd` in both places so every release and local
`make build` ships both binaries. The `Dockerfile` is intentionally left alone:
its distroless image is `vsockd`'s host runtime, while the supervisor runs
inside the enclave image built in the Node app repo.

The binary is `supervisor` (from `./cmd/supervisor`).

## Context (from discovery)

- `.github/workflows/nightly.yml` — `build` job (matrix `goarch: [amd64,
  arm64]`) runs vet + test, then a single `go build … -o
  dist/vsockd-linux-${GOARCH} ./cmd/vsockd`, a `sha256sum`, and an
  `upload-artifact` (name `vsockd-linux-${arch}`, path
  `dist/vsockd-linux-${arch}*`). The `release` job downloads all artifacts
  (`merge-multiple: true`) into `dist/` and attaches four vsockd files to the
  rolling `nightly` release via `softprops/action-gh-release@v3`.
- `Makefile` — `BINARY := vsockd`; `build` does `CGO_ENABLED=0 go build
  $(BUILDFLAGS) -o $(BINARY) ./cmd/vsockd`; `clean` removes `$(BINARY)` and
  `dist/`.
- README has no references to the nightly release or downloadable binaries, so
  no doc updates are required.

## Development Approach

- This is an infra change (CI YAML + Makefile), so verification is not Go unit
  tests: it is **`actionlint`** on the workflow (required by repo convention
  after editing any `.github/workflows/*.yml`), local build checks for both
  binaries and both arches, and a post-merge `workflow_dispatch` run that
  confirms the release attaches all eight files.
- Make small, focused changes; keep `supervisor` wiring a mirror of `vsockd`.
- Maintain backward compatibility: the existing vsockd artifacts/release files
  keep their names.

## Implementation Steps

### Task 1: Add supervisor to the nightly workflow

- [x] in the `build` job's "Build static binary" step, add a second `go build`
      mirroring vsockd:
      `go build -trimpath -ldflags "-s -w" -o "dist/supervisor-linux-${GOARCH}" ./cmd/supervisor`.
- [x] in the "Checksum" step, also checksum the supervisor binary
      (`sha256sum supervisor-linux-${GOARCH} > supervisor-linux-${GOARCH}.sha256`).
- [x] broaden the `upload-artifact` step to carry both binaries per arch: rename
      the artifact to `binaries-linux-${{ matrix.goarch }}` and set the path to
      `dist/*-linux-${{ matrix.goarch }}*` (keep `if-no-files-found: error`).
      (The `release` job downloads with `merge-multiple: true`, so the artifact
      rename does not affect it.)
- [x] in the `release` job's `files:` list, add the four supervisor files:
      `dist/supervisor-linux-amd64`, `…amd64.sha256`,
      `dist/supervisor-linux-arm64`, `…arm64.sha256`.
- [x] run `actionlint .github/workflows/nightly.yml` — must report no errors.
- [x] verify the build command locally for both arches:
      `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /tmp/supervisor-amd64 ./cmd/supervisor`
      and the same for `arm64` — both must succeed.

### Task 2: Add supervisor to the Makefile

- [x] update `build` to produce both binaries (vsockd and supervisor), e.g. add
      a `SUPERVISOR := supervisor` var and a second
      `CGO_ENABLED=0 go build $(BUILDFLAGS) -o $(SUPERVISOR) ./cmd/supervisor`.
- [x] update `clean` to also `rm -f` the supervisor binary.
- [x] verify: `make build` produces both `vsockd` and `supervisor`; `make clean`
      removes both. Run `make build && ls -l vsockd supervisor && make clean`.

### Task 3: Verify acceptance criteria

- [x] re-run `actionlint` on the workflow and `go vet ./...` — both clean.
- [x] confirm `make build` builds both binaries and `git status` shows no stray
      tracked artifacts (both binary names are git-ignored).
- [x] confirm the workflow's `release` `files:` list now references all eight
      artifacts (4 vsockd + 4 supervisor).

## Technical Details

- **Artifact naming**: per-arch artifact renamed `vsockd-linux-${arch}` →
  `binaries-linux-${arch}` so one artifact carries both binaries + checksums
  for that arch. Safe because `release` merges all artifacts into `dist/` and
  selects files by explicit path.
- **Release files** after the change (8 total):
  `vsockd-linux-{amd64,arm64}`(+`.sha256`),
  `supervisor-linux-{amd64,arm64}`(+`.sha256`).
- **Unchanged**: the `check` job, build matrix, vet/test steps, Go setup, and
  the `Dockerfile`.

## Post-Completion

*Requires a CI run — informational only.*

- Trigger the workflow via **`workflow_dispatch`** (it builds regardless of the
  nightly tag) and confirm: both binaries build for amd64 and arm64, artifacts
  upload, and the rolling `nightly` release ends up with all eight files.
- Confirm the published `supervisor-linux-*` binaries run (`--version`/`-h`) on
  a matching arch.
