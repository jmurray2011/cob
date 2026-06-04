# Changelog

All notable changes to `cob` are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- `rm --force` no longer deletes a Published version when it could not verify
  whether other repos in the domain hold promoted copies. A transient error
  probing a repo was previously read as "no copy", silently bypassing the
  `--everywhere` consent gate and leaving a downstream chain-of-evidence
  dangling. Unverifiable repos now block the delete (use `--force --everywhere`
  to override).
- `cob pull` on Windows no longer misreads a drive-letter manifest path such as
  `C:\dir\cob-manifest.yaml` as coordinates `C` plus an inline filter.
- `COB_*` boolean environment variables: `COB_QUIET=no` (and `=off`/`=n`) now
  means "not quiet". Previously any unrecognized value silently forced the flag
  to `true`. The accepted spellings now include `yes`/`no`/`on`/`off`
  (case-insensitive); an unrecognized value is treated as unset.
- `pull`'s `FetchAssetInfo` and `promote`'s `ListAssetsToPromote` now bound
  their pagination with the same safety cap as every other CodeArtifact
  listing, so a misbehaving endpoint that returns an endless page token can no
  longer loop or exhaust memory.
- Asset names containing multibyte characters (CJK, emoji) no longer truncate
  into invalid UTF-8 in the live transfer view.
- An inline asset filter with blank elements (`coords:a.bin,,b.bin`) no longer
  warns about a phantom empty asset name.

### Changed

- `cob tree` and `cob ls -R` now walk the hierarchy with a bounded number of
  goroutines (one pool of `treeWalkConcurrency`) instead of spawning one
  goroutine per node. A deep walk over a large organization no longer stands up
  an unbounded fan-out. Output content and ordering are unchanged.
- `cob init` writes the generated manifest atomically with `O_CREATE|O_EXCL`
  when `--force` is not given, closing the previous stat-then-create race and
  refusing to create through a symlink onto an unintended target. `--force`
  still truncates in place.
- The "no current package" error now names the command that needs coordinates.

### Added

- `CONTRIBUTING.md` documenting the validate gate and the tests-first workflow.
- This changelog.

## [0.0.1]

- Initial release.

[Unreleased]: https://github.com/jmurray2011/cob/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/jmurray2011/cob/releases/tag/v0.0.1
