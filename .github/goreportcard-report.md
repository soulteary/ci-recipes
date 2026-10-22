# Go Report Card

**Grade: A+** (95.6%)

| Metric | Value |
| ------ | ----- |
| Files | 54 |
| Issues | 20 |

## Checks

| Check | Score |
| ----- | ----- |
| gofmt | 100% |
| go_vet | 100% |
| ineffassign | 100% |
| gocyclo | 62% |
| license | 100% |
| misspell | 100% |

## Issues

### gocyclo

- `internal/recipes/wordpress/published/published.go`
  - Line 66: cyclomatic complexity 23 for function ExecuteWithOptions
- `internal/app/app_test.go`
  - Line 89: cyclomatic complexity 21 for function TestEveryPublicCommandDispatchesAndAdvertisesUsage
- `internal/recipes/wordpress/release/release.go`
  - Line 81: cyclomatic complexity 49 for function ExecuteWithOptions
  - Line 367: cyclomatic complexity 24 for function (*DockerRegistryResolver).Resolve
- `internal/recipes/stargate/checks/contracts.go`
  - Line 95: cyclomatic complexity 34 for function contractViolations
  - Line 380: cyclomatic complexity 28 for function checkAPISections
  - Line 597: cyclomatic complexity 24 for function checkShellDocumentationLines
  - Line 642: cyclomatic complexity 21 for function checkCompose
  - Line 495: cyclomatic complexity 16 for function checkForbiddenDocumentation
- `internal/recipes/grantseal/patch_coverage.go`
  - Line 35: cyclomatic complexity 28 for function runPatchCoverage
  - Line 185: cyclomatic complexity 22 for function parseAddedLines
- `internal/recipes/grantseal/report.go`
  - Line 354: cyclomatic complexity 25 for function runGenerateQualityDocs
  - Line 73: cyclomatic complexity 19 for function deriveReportEnvironment
- `internal/recipes/wordpress/evidence/evidence.go`
  - Line 19: cyclomatic complexity 24 for function Execute
- `internal/recipes/errortracer/release/archive.go`
  - Line 18: cyclomatic complexity 19 for function writeTarGzip
  - Line 111: cyclomatic complexity 16 for function writeZip
- `internal/recipes/stargate/checks/selfchecks.go`
  - Line 109: cyclomatic complexity 18 for function validateReleaseWorkflow
- `internal/recipes/errortracer/release/release.go`
  - Line 181: cyclomatic complexity 48 for function execute
- `internal/recipes/grantseal/archive.go`
  - Line 579: cyclomatic complexity 44 for function validateZipContainer
  - Line 1065: cyclomatic complexity 24 for function readImageTarWithLimit
  - Line 727: cyclomatic complexity 24 for function validateZipExtra
  - Line 940: cyclomatic complexity 19 for function parseManifestFile
  - Line 870: cyclomatic complexity 19 for function normalizeArchivePath
  - Line 827: cyclomatic complexity 19 for function validateReleaseMember
  - Line 1305: cyclomatic complexity 18 for function normalizedImageLinkTarget
  - Line 1388: cyclomatic complexity 17 for function baseImageMemberAllowed
  - Line 210: cyclomatic complexity 16 for function readTarGzipMembersWithLimits
- `internal/recipes/grantseal/docs.go`
  - Line 145: cyclomatic complexity 31 for function runDocConsistency
  - Line 24: cyclomatic complexity 22 for function runDocLanguage
- `internal/recipes/stargate/checks/htpasswd.go`
  - Line 32: cyclomatic complexity 29 for function shellCommandFragments
  - Line 598: cyclomatic complexity 28 for function envSplitWords
  - Line 266: cyclomatic complexity 19 for function shellLiteralWords
  - Line 789: cyclomatic complexity 17 for function parseIoniceOption
  - Line 159: cyclomatic complexity 17 for function envSplitOperandAtQuote
  - Line 536: cyclomatic complexity 16 for function unwrapEnvWords
  - Line 338: cyclomatic complexity 16 for function htpasswdCommandWords
- `internal/recipes/stargate/release/release.go`
  - Line 584: cyclomatic complexity 28 for function executeReconcile
  - Line 295: cyclomatic complexity 19 for function executePrepare
  - Line 236: cyclomatic complexity 19 for function extractNotes
  - Line 514: cyclomatic complexity 17 for function executePublish
  - Line 433: cyclomatic complexity 16 for function releaseAssets
- `internal/recipes/grantseal/sensitive.go`
  - Line 142: cyclomatic complexity 18 for function (*sensitiveScanner).scanTarget
  - Line 85: cyclomatic complexity 17 for function (*sensitiveScanner).scanTracked
  - Line 279: cyclomatic complexity 16 for function fileContainsPrivateKeyHeader
- `internal/recipes/stargate/checks/markdown.go`
  - Line 422: cyclomatic complexity 35 for function fencedBlockViolations
  - Line 194: cyclomatic complexity 25 for function parseListMarker
  - Line 797: cyclomatic complexity 23 for function multilineReferenceLabelLines
  - Line 573: cyclomatic complexity 22 for function referenceDestinationPrefix
  - Line 341: cyclomatic complexity 22 for function interruptsParagraph
  - Line 296: cyclomatic complexity 17 for function htmlBlockStart
- `internal/recipes/wordpress/cosign/cosign.go`
  - Line 65: cyclomatic complexity 29 for function ExecuteWithOptions
- `internal/recipes/grantseal/execute.go`
  - Line 85: cyclomatic complexity 21 for function execute
- `internal/recipes/errortracer/release/release_test.go`
  - Line 847: cyclomatic complexity 20 for function assertTarGzip
  - Line 92: cyclomatic complexity 20 for function TestExecuteBuildsCompleteReleaseBundle
- `internal/recipes/stargate/checks/checks.go`
  - Line 216: cyclomatic complexity 25 for function archiveRevision
  - Line 94: cyclomatic complexity 20 for function executeWithOptions

---

_Generated by [Go Report Card](https://github.com/soulteary/goreportcard-action)._
