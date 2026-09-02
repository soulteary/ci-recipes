# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

- Add a single Go CLI for active CI scripts from Stargate, Docker SQLite
  WordPress, Error-Tracer, and Grantseal.
- Add deterministic release archives, bounded registry clients, atomic report
  writes, and fail-closed policy checks.
- Replace shell self-tests with isolated Go unit and integration tests.
- Document audited source snapshots and inactive-script exclusions.
- Reject non-canonical or hidden archive payloads, unsafe cross-platform paths,
  unverified registry manifest bodies, and release-asset symlinks.

### Fixed

- Accept Git's canonical global PAX commit header when creating isolated
  Stargate snapshots while continuing to reject arbitrary PAX metadata.
- Detect Stargate documentation examples that enable `htpasswd` batch-password
  mode after earlier options, root prompts, or common command wrappers without
  crossing shell command or comment boundaries.
- Reject symlinked Grantseal release-archive candidates instead of silently
  treating the archive directory as empty.
