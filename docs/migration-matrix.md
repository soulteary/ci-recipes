# Active CI script migration matrix

The inventory is intentionally tied to immutable source snapshots. A row that
says only “Go tests” means the shell self-test behavior is regression coverage
and does not add another executable. A row naming both a command and Go tests
represents a checked-out-repository contract that remains callable by CI and
also has regression coverage.

## Stargate

Source: `soulteary/stargate@e129b7407e2353c4d6c6a2f12abed5b35d77f038`

| Shell file | Go replacement |
|---|---|
| `.github/scripts/check-doc-contracts.sh` | `stargate check-doc-contracts` |
| `.github/scripts/extract-release-notes.sh` | `stargate extract-release-notes` |
| `.github/scripts/prepare-release-notes.sh` | `stargate prepare-release-notes` |
| `.github/scripts/plan-release-aliases.sh` | `stargate plan-release-aliases` |
| `.github/scripts/publish-github-release.sh` | `stargate publish-github-release` |
| `.github/scripts/reconcile-release-aliases.sh` | `stargate reconcile-release-aliases` |
| `.github/scripts/test-doc-contracts.sh` | Go tests for document contracts |
| `.github/scripts/test-go-version-contract.sh` | `stargate check-go-version-contract` plus Go regression tests |
| `.github/scripts/test-release-notes.sh` | Go release-note tests |
| `.github/scripts/test-release-workflow.sh` | `stargate check-release-workflow` plus Go release tests |
| `.github/scripts/test-start-local.sh` | Go local-launcher tests |
| `start-local.sh` (transitive test target) | `stargate local-run` |

Stargate check commands search upward for the repository marker, while release
commands use their explicit paths relative to the current directory. The alias
commands calculate from a point-in-time snapshot and provide neither an
internal distributed lock nor compare-and-swap for registry/GitHub mutations;
the calling release workflow must serialize alias jobs.

## Docker SQLite WordPress

Source: `soulteary/docker-sqlite-wordpress@ca92cc4f061a9385a16bf175c5871f5c2c71d5c4`

| Shell file | Go replacement |
|---|---|
| `scripts/validate-buildx-evidence.sh` | `docker-sqlite-wordpress validate-buildx-evidence` |
| `scripts/validate-release.sh` | `docker-sqlite-wordpress validate-release` |
| `scripts/verify-cosign-signature.sh` | `docker-sqlite-wordpress verify-cosign-signature` |
| `scripts/verify-published-release.sh` | `docker-sqlite-wordpress verify-published-release` |

The existing shell tests for these scripts were used as behavior fixtures and
expanded into table, subprocess, and fake-HTTP tests. Buildx evidence validation
covers both SPDX SBOM and SLSA provenance records for the expected platforms.

## Error-Tracer

Source: `soulteary/Error-Tracer@7490e989e0a932292f8f2b0a9088b3d243d6c060`

| Shell file | Go replacement |
|---|---|
| `scripts/build-release.sh` | `error-tracer build-release` |

## Grantseal

Source: `soulteary/grantseal@ff359454cd26fd691dd67a77f06faacfecbd1d69`

| Shell file | Go replacement |
|---|---|
| `scripts/check-archive-allowlist.sh` | `grantseal check-archive-allowlist` |
| `scripts/check-sensitive-files.sh` | `grantseal check-sensitive-files` |
| `scripts/check-patch-coverage.sh` | `grantseal check-patch-coverage` |
| `scripts/check-doc-language.sh` | `grantseal check-doc-language` |
| `scripts/check-doc-consistency.sh` | `grantseal check-doc-consistency` |
| `scripts/check-writeback-allowlist.sh` | `grantseal check-writeback-allowlist` |
| `scripts/inject-report-environment.sh` | `grantseal inject-report-environment` |
| `scripts/generate-quality-docs.sh` | `grantseal generate-quality-docs` |
| five `*-selftest.sh` files | Go tests for the corresponding recipes |

`check-doc-language` also accepts `--allowlist FILE`, and
`generate-quality-docs` accepts an optional repository root.

## Audited and excluded

| Source snapshot | Script | Reason |
|---|---|---|
| `soulteary/apt-proxy@1f7144d6b814e4f1dd4fe249043a907367a11bd5` | `scripts/add_license_header.sh` | No workflow or Makefile target invokes it. |
| `soulteary/webhook@413e876646ae61d183aba8825b8ef7de7c0596a5` | `scripts/test-coverage.sh` | No workflow invokes it; it is documented as a local helper. |

Runtime entrypoints, examples, Bash test files outside the linked script
directories, and inline workflow shell blocks are outside this migration.
