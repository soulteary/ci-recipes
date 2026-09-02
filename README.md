# CI Recipes

`ci-recipes` replaces repository-specific CI shell programs with one tested,
cross-platform Go binary. Each recipe keeps the success, failure, output, and
side-effect contract used by valid CI invocations while removing shell
interpolation, implicit pipelines, temporary-file leaks, and fail-open error
handling. The unified CLI reports command-selection and missing-argument
errors with exit code 2; recipe-level validation and operation failures retain
their documented codes.

The initial migration covers only scripts that are reachable from the current
GitHub Actions workflows of Stargate, Docker SQLite WordPress, Error-Tracer,
and Grantseal. Shell self-test behavior is covered by Go regression tests.
Stargate's Go-version and release-workflow self-tests also enforce contracts on
the checked-out repository, so that behavior remains available as public
`check-*` commands; the other test scripts do not become executables. Scripts
in `apt-proxy/scripts` and `webhook/scripts` were audited but are not included
because their current workflows do not execute them.

## Install

Pin a release tag or full commit in CI:

```console
go install github.com/soulteary/ci-recipes/cmd/ci-recipes@<tag-or-commit>
ci-recipes help
```

For local development:

```console
go build -trimpath -o ./bin/ci-recipes ./cmd/ci-recipes
go test ./...
```

The module requires Go 1.22 or newer. The recipe implementation uses the Go
standard library. A recipe invokes an external program only when that program
is the actual trust or build boundary: `go`, `git`, `gh`, `docker`, or
`cosign`. No recipe launches a shell.

## Commands

Run `ci-recipes help` for a command overview; the public syntax is listed
below. Commands retain the old script basename so workflow replacements stay
obvious. Unless stated below, run repository-specific recipes from that
repository's root, matching GitHub Actions' default working directory.
Stargate check commands, including
`local-run`, search upward for `src/cmd/stargate`; Stargate release commands do
not discover a root and resolve relative file arguments from the current
directory. `error-tracer build-release` searches upward for the Error-Tracer
repository root. Docker SQLite WordPress and Grantseal recipes use the current
directory or their explicit path arguments.

### Stargate

```console
ci-recipes stargate check-doc-contracts [BASE_SHA]
ci-recipes stargate check-go-version-contract
ci-recipes stargate extract-release-notes TAG OUTPUT [CHANGELOG]
ci-recipes stargate prepare-release-notes TAG OUTPUT [CHANGELOG]
ci-recipes stargate plan-release-aliases TAG RELEASE_TAGS_FILE
ci-recipes stargate publish-github-release TAG NOTES_FILE DIST_DIR
ci-recipes stargate reconcile-release-aliases TAG IMAGE
ci-recipes stargate check-release-workflow
ci-recipes stargate local-run [-port PORT]
```

The release commands preserve `GITHUB_REPOSITORY` and
`ALLOW_EXISTING_RELEASE_NOTES`. GitHub Release operations use the authenticated
`gh` process, and image alias reconciliation uses Docker Buildx without shell
pipelines. Alias planning and reconciliation operate on a point-in-time tag or
release snapshot; the binary does not take a distributed lock and registry or
GitHub updates do not use compare-and-swap. Callers must serialize alias jobs,
as Stargate's release workflow does with its dedicated concurrency group.

### Docker SQLite WordPress

```console
ci-recipes docker-sqlite-wordpress validate-release RELEASE_VERSION [--verify-upstream]
ci-recipes docker-sqlite-wordpress validate-buildx-evidence SPDX PLATFORMS_JSON < evidence.json
ci-recipes docker-sqlite-wordpress validate-buildx-evidence SLSA PLATFORMS_JSON < evidence.json
ci-recipes docker-sqlite-wordpress verify-cosign-signature IMAGE IDENTITY ISSUER
ci-recipes docker-sqlite-wordpress verify-published-release RELEASE_VERSION
```

The evidence command validates a non-empty per-platform SPDX SBOM or SLSA
provenance object for exactly the expected platform set. Registry verification
uses bounded HTTP requests and retry policy. Cosign verification deliberately
continues to call `cosign`: replacing the verifier would change the trust
contract. `COSIGN_VERIFY_ATTEMPTS` and `COSIGN_VERIFY_DELAY_SECONDS` remain
supported.

### Error-Tracer

```console
ci-recipes error-tracer build-release VERSION [OUTPUT_DIR]
```

The recipe builds the existing six-target matrix, produces deterministic tar,
gzip, and zip metadata, verifies the Linux AMD64 binary, and publishes the
completed staging directory with a single same-filesystem rename; it refuses an
existing output path. `GITHUB_SHA`, `SOURCE_DATE_EPOCH`, and `BUILD_DATE` keep
their previous precedence. As with the original release script, this command
must run on Linux AMD64 because it executes the generated Linux AMD64 binary as
a metadata smoke test.

### Grantseal

```console
ci-recipes grantseal check-archive-allowlist [DIR]
ci-recipes grantseal check-archive-allowlist --image IMAGE
ci-recipes grantseal check-archive-allowlist --manifest FILE [BASE]
ci-recipes grantseal check-sensitive-files [PATH ...]
ci-recipes grantseal check-patch-coverage PROFILE [BASE_REF] [THRESHOLD]
ci-recipes grantseal check-doc-language [ROOT] [--allowlist FILE]
ci-recipes grantseal check-doc-consistency [ROOT]
ci-recipes grantseal check-writeback-allowlist
ci-recipes grantseal inject-report-environment [REPORT_JSON]
ci-recipes grantseal generate-quality-docs [REPO_ROOT]
```

Policy failures use exit code 1. Invalid input or unavailable required
infrastructure uses exit code 2. Unlike the former process-substitution and
`|| true` paths, Git, coverage, archive, document, and JSON parse failures are
fail-closed. Report and documentation updates are staged in same-directory
temporary files and then renamed; the two quality-document updates attempt a
rollback if the second commit fails. Replacement atomicity follows the host
filesystem's rename semantics. In particular, an existing destination may
make the rename fail on Windows, which is reported instead of falling back to
an in-place partial write. Archive inspection is cancellation-aware and limits
compressed inputs to 256 MiB and expanded archive/image data to 1 GiB; image
manifest inputs are limited to 64 MiB. Non-canonical ZIP/tar metadata, hidden
trailing payloads, and cross-platform absolute paths fail closed. The accepted
artifact subset is canonical `tar.gz` plus stored/deflated ZIP entries; ZIP64,
alternate ZIP compression, named/commented gzip headers, and opaque PAX or ZIP
metadata are intentionally rejected.

## Migration inventory

[`docs/migration-matrix.md`](docs/migration-matrix.md) records the audited
source commits, each old script, its replacement, and the scripts excluded by
the active-CI boundary.

## Reliability

Recipe logic is separated from process, HTTP, clock, sleep, and filesystem
boundaries. Tests use temporary repositories, deterministic archives, fake
registries, and fake external commands; they do not require live network
access.

CI runs `go test -count=1 ./...` and a `-trimpath` build on Ubuntu with Go
1.22.x and Go 1.27.x, and on macOS and Windows with Go 1.27.x. Its Ubuntu Go
1.27.x quality job formats the tree and requires a clean diff, then runs the
race detector and `go vet`. Equivalent local checks are:

```console
gofmt -w .
git diff --exit-code
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build -trimpath ./cmd/ci-recipes
```

## License

Apache License 2.0; see [`LICENSE`](LICENSE).
