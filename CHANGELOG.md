# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

- Add a single Go CLI for active CI scripts from Stargate, Docker SQLite
  WordPress, Error-Tracer, and Grantseal.
- Add `runner-fleet check-version-consistency` and
  `runner-fleet check-docs-structure`, replacing the two shell checks Runner
  Fleet's `CI (Consistency)` workflow and `make check` target run. Both shell
  originals reported success in situations where they had checked nothing: a
  `git ls-files` status swallowed by a pipeline that ended in `|| true`, an
  unquoted `for` loop that dropped any path containing a space, and an absent
  documentation tree that compared nothing and still printed its all-clear. All
  three are regression tests, as is a fence-tracking defect that knew backticks
  but not tildes and so both mismodelled tilde-fenced documents and reported
  structurally identical translations as wrong.
- Read Runner Fleet's repository-owned check settings from
  `scripts/ci-recipes.conf`, or from `--config FILE`: the version baseline path
  and regex, the ignore marker, the scanned Git pathspecs, and the documentation
  root, language and file lists. The defaults reproduce the audited shell
  behavior, so the file is optional, and an unknown key or a configuration that
  would compare nothing is an error rather than a silent fall back.
- Add deterministic release archives, bounded registry clients, atomic report
  writes, and fail-closed policy checks.
- Replace shell self-tests with isolated Go unit and integration tests.
- Document audited source snapshots and inactive-script exclusions.
- Reject non-canonical or hidden archive payloads, unsafe cross-platform paths,
  unverified registry manifest bodies, and release-asset symlinks.

### Fixed

- Describe the centralized Grantseal quality-doc generator in its generated
  English and Chinese provenance markers instead of naming the removed wrapper.
- Accept Git's canonical global PAX commit header when creating isolated
  Stargate snapshots while continuing to reject arbitrary PAX metadata.
- Detect Stargate documentation examples that enable `htpasswd` batch-password
  mode after earlier options, root prompts, or command wrappers such as
  `sudo`, `env`, `command`, and execution-form `ionice`, without crossing shell
  command or comment boundaries.
- Reject symlinked Grantseal release-archive candidates instead of silently
  treating the archive directory as empty.
- Preserve Stargate shell logical-line state so comments after an unquoted
  continuation are not misread as Markdown root prompts.
