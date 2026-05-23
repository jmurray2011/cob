# cob

Assemble AWS CodeArtifact packages from remote sources. No local artifacts required.

cob reads a YAML manifest that declares what goes into a package by pointing at S3 objects, other CodeArtifact packages, or local files. Assets stream from source to CodeArtifact in constant memory -- no size limit, and the full asset is never held in RAM (it spills through a transient temp file; see Known limitations).

## Install

```bash
go install github.com/jmurray2011/cob/cmd/cob@latest
```

Or build from source:

```bash
go build -o cob ./cmd/cob
```

`go install` needs a Go toolchain at the version in [go.mod](go.mod) or newer
(currently Go 1.25 -- chosen so a build picks up the latest standard-library
security patches, which `govulncheck` enforces in CI).

### Verifying release binaries

Release archives ship a `checksums.txt` signed with [cosign](https://github.com/sigstore/cosign) keyless signing (no key to trust -- the signature is tied to the GitHub Actions release workflow's identity and logged in the public Rekor transparency log). To verify a download:

```bash
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity-regexp '^https://github.com/jmurray2011/cob/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum --check checksums.txt   # then check the binary against the verified list
```

## Quick start

Write a manifest that describes your package:

```yaml
# my-package.yaml
domain: my-domain
repository: dev
namespace: my-namespace
package: my-package

sources:
  app:      s3://my-bucket/builds/app-${VERSION}.tar.gz
  config:   s3://my-bucket/configs/app-config.yaml
  baseline: ca://acme/shared/common-configs@1.0.0/defaults.yaml
  local:    ./local-overrides.yaml

promote:
  stages: [dev, staging, prod]
```

Publish it:

```bash
cob publish my-package.yaml --version 2.1.0
```

## Commands

### publish

Reads a manifest, resolves variables, pulls from each source, publishes to CodeArtifact.

```bash
cob publish my-package.yaml --version 2.1.0
cob publish my-package.yaml --version 2.1.0 --dry-run   # verify sources, don't publish
cob publish my-package.yaml --version 2.1.0 --force      # overwrite existing version
cob publish my-package.yaml --version 2.1.0 --resume     # finish an interrupted publish
cob publish my-package.yaml --version 2.1.0 --yes        # skip confirmation prompt
COB_VERSION=2.1.0 cob publish my-package.yaml            # version from env
```

Flags: `--version`, `--dry-run`, `--force`, `--resume`, `--yes`, `--concurrency`

If a publish is interrupted partway, the version is left **Unfinished** in CodeArtifact. `--resume` continues it from the same manifest: it uploads only the assets not already present and then writes the finalizer -- no re-uploading what already landed. A present asset is taken as complete (CodeArtifact validated its SHA-256 on the original upload); if a source changed since the interrupted run, use `--force` instead. `--resume` and `--force` are mutually exclusive.

`publish` and `promote` prompt before mutating. With no TTY (CI, pipes) they
do **not** silently proceed -- they refuse unless `--yes` is given, so a
pipeline can't delete or overwrite by accident. Pass `--yes` in CI.
`--dry-run` never prompts.

Every publish also writes a `cob-provenance.json` asset (the finalizer) --
see [Provenance](#provenance).

### pull

Downloads assets to a local directory. Works with a manifest or compact coordinates.

```bash
# With a manifest -- pull all assets
cob pull my-package.yaml --version 2.1.0 --output ./assets/

# Ad-hoc -- grab one asset by its stored name (the source filename)
cob pull my-domain/dev/my-namespace/my-package@2.1.0 app-2.1.0.tar.gz

# Ad-hoc -- grab all assets
cob pull my-domain/dev/my-namespace/my-package@2.1.0 --output ./assets/

# Pull the latest published version
cob pull my-domain/dev/my-namespace/my-package@latest --output ./assets/

# Pull specific assets by stored name
cob pull my-domain/dev/my-namespace/my-package@2.1.0 --assets app-2.1.0.tar.gz,app-config.yaml
```

Skips files that already exist with a matching SHA-256.

A **whole-package pull into a directory** (no single asset, no `--assets`)
also writes a `cob-manifest.yaml` next to the assets -- the same manifest
[`cob manifest`](#manifest) would produce (reconstructed from provenance, or
inferred). So `cob pull <coords> --output ./d/` gives you the assets, their
`cob-provenance.json`, and a manifest to re-publish or inspect from. Partial
or single-asset pulls don't (the manifest would misrepresent the package);
a manifest hiccup only warns -- the assets are already down.

Flags: `--version`, `--output`, `--assets`, `--concurrency`

### promote

Copies a package version between repositories. No local disk involved.

```bash
# Compact coordinates
cob promote my-domain/dev/my-namespace/my-package@2.1.0 --to staging

# With manifest -- source repo inferred from promote.stages
cob promote my-package.yaml --version 2.1.0 --to staging   # dev -> staging
cob promote my-package.yaml --version 2.1.0 --to prod      # staging -> prod

# Promote whatever is latest in the source repo
cob promote my-domain/dev/my-namespace/my-package@latest --to staging

# Preview the move without copying anything
cob promote my-domain/dev/my-namespace/my-package@2.1.0 --to staging --dry-run

# Finish an interrupted promote without re-copying what already landed
cob promote my-domain/dev/my-namespace/my-package@2.1.0 --to staging --resume
```

Flags: `--to` (required), `--version`, `--force`, `--resume`, `--yes`, `--dry-run`, `--concurrency`

`--resume` works the same way as on `publish`: it requires the destination version to be **Unfinished** (left behind by a partial promote), copies only assets not already present, then writes the finalizer. Mutually exclusive with `--force`.

### rm

Deletes a package version. cob treats published versions as immutable, so
the command is gated in three tiers:

```bash
# Tier 1 (default): clean up a failed/abandoned publish
cob rm my-domain/dev/my-namespace/my-package@2.1.0-rc1

# Tier 2 (--force): delete a real Published version with no downstream copies
cob rm my-domain/dev/my-namespace/my-package@2.1.0 --force

# Tier 3 (--force --everywhere): delete even when other repos have promoted from this version
cob rm my-domain/dev/my-namespace/my-package@2.1.0 --force --everywhere
```

`rm` refuses `@latest` as a typo-shield — the only destructive verb makes
you name the bytes explicitly. With `--force` on a Published version, cob
probes every other repo in the same domain; if any holds the same version
(meaning a `promote` once recorded `from: <this-repo>`), the deletion
refuses and lists those repos. `--force --everywhere` overrides; the
confirm prompt then names the chains that will dangle.

Deletion destroys the `cob-provenance.json` along with the assets;
there is no soft-delete or archive.

Flags: `--force`, `--everywhere`, `--yes`

### ls

Drill into CodeArtifact at any level:

```bash
cob ls my-domain/dev                                            # list packages
cob ls my-domain/dev/my-namespace/my-package                    # list versions
cob ls my-domain/dev/my-namespace/my-package@2.1.0              # list assets
cob ls 'my-domain/*/my-namespace/my-package@2.1.0'              # promotion status across repos
cob ls my-domain/dev/my-namespace/my-package@latest             # assets in latest version
```

(Quote `*` for zsh; bash leaves an unmatched literal `*` alone.)

For multi-level discovery use `-R` (flat, fully-qualified) or [`tree`](#tree)
(indented tree):

```bash
cob ls -R                                                       # every package, one per line
cob ls -R my-domain/dev --depth versions                        # every version under a repo
cob ls -R my-domain/dev/my-namespace/my-package --depth assets  # every asset under a package
```

`--depth` accepts `domains|repos|packages|versions|assets`; the default is
`packages`, with one extra level when you target a specific node (so
`cob ls -R my-domain/dev` lists packages without spelling out `--depth packages`,
and targeting a single package descends one level into versions).

Flags: `-R`/`--recursive`, `--depth`

### tree

Tree-shaped view of the same walk `ls -R` produces — useful for "what do
we have?" exploration. Branches that can't be listed (denied repo, throttled
call) are marked inline with `!` and don't abort the rest of the walk.

```bash
cob tree                                            # everything down to packages
cob tree my-domain                                  # one domain
cob tree my-domain/dev --depth versions             # one repo down to versions
cob tree my-domain/dev/my-namespace/my-package      # versions of a package (default-bumped)
cob tree my-domain/dev/my-namespace/my-package --depth assets
```

Default depth is `packages`. Targeting a node and omitting `--depth`
descends one level into it. JSON mode emits a nested tree
(`{name, kind, path, meta, children, error}`) rather than the flat array
`ls -R --json` produces — pick whichever shape your consumer wants.

Flags: `--depth`

### resolve

Resolves the latest published version and prints the version string. Designed for scripting.

```bash
# Print just the version string
cob resolve my-domain/dev/my-namespace/my-package
# -> 2.1.0

# Use in scripts
VERSION=$(cob resolve my-domain/dev/my-namespace/my-package)
cob pull my-domain/dev/my-namespace/my-package@$VERSION --output ./assets/

# JSON output
cob resolve my-domain/dev/my-namespace/my-package --json
# -> {"package": "my-namespace/my-package", "repository": "my-domain/dev", "version": "2.1.0"}
```

Resolution is by publication timestamp, not semver.

### init

Prints a starter manifest to stdout — placeholders that pass `cob diff`
lint out of the box, plus inline examples for each source type. The optional
coordinates argument fills in any subset of domain/repository/namespace/
package; the rest stay as placeholders so you can fill them in as you
iterate. No AWS calls.

```bash
# generic template
cob init > my-package.yaml

# fill in identifiers up front
cob init acme/dev/tools/my-app > my-package.yaml

# partial: only the domain
cob init acme > my-package.yaml

# bare schema — no comments, no promote stages
cob init acme/dev/tools/my-app --minimal > my-package.yaml
```

Flags: `--minimal`

### log

Read-only print of a version's chain of evidence — who published it, who
promoted it, when, and where — plus per-asset origins (where each file
physically came from, recursing through `ca://` upstreams). No integrity
check; that's what [`diff <coords>`](#diff) is for. Useful when you want
the history without paying for the asset listing and hash comparison.

```bash
cob log my-domain/dev/my-namespace/my-package@2.1.0
cob log my-domain/dev/my-namespace/my-package@latest

# Machine-readable: emits the full Provenance struct as JSON
cob log my-domain/dev/my-namespace/my-package@2.1.0 --json

# Surface chain references that point at deleted versions
cob log my-domain/staging/my-namespace/my-package@2.1.0 --check-references
```

`--check-references` probes each chain event's referenced repository
(`publish.repository`, `promote.from`/`promote.to`) once and annotates
inline:

- `(deleted)` — the referenced version no longer exists in that repo
  (e.g. it was removed by `cob rm --force --everywhere`)
- `(?)` — the probe itself failed (auth, throttle, network); distinct
  from `(deleted)` so you don't mistake "couldn't check" for "definitely
  gone"

Adds one `VersionStatus` call per **unique** referenced repository (the
probe set dedupes), so a long chain that bounces between two repos still
costs only two probes. Text-mode only — a JSON consumer can run their
own loop over `prov.chain[]`.

Exits with an error if the version has no `cob-provenance.json` (a non-cob
publisher or pre-provenance version). For those, `cob diff <coords>`
still won't help (no provenance to compare against), but CodeArtifact's
stored asset hashes can be inspected via `cob ls <coords>@<v>`.

A note on the dangling-chain situation `--check-references` surfaces: the
destination's provenance is **self-contained** (`promote` carries the
source's chain forward and appends its own event, and `reconcilePromotedAssets`
preserves each asset's original Source URI and S3 Origin record). So a
deleted upstream affects only the ability to follow a `from:` link by
hand — it does not affect `cob diff <coords>` self-integrity, manifest-
vs-published diff, or `--deep` rehash, all of which work entirely from
the destination's own records.

### diff

`diff` is cob's only comparison verb. Five modes, picked from the
positional shape; none mutate; exit code is 0 if identical, non-zero on
any drift.

Match rows are terse by default: glyph, name, size, method (one line).
Mismatch rows expand to the full audit form (URI, size, full SHA-256 for
both sides) — that's where the bytes matter. Pass `-v` / `--verbose` to
put the URI and full hash on every row, not just mismatches.

| Mode                       | Invocation                            | What it answers                            |
|----------------------------|---------------------------------------|--------------------------------------------|
| Manifest lint (offline)    | `cob diff <m.yaml>`                   | Is this manifest well-formed?              |
| Manifest vs published      | `cob diff <m.yaml> --version X`       | Does this manifest still produce @X?       |
| Self-integrity             | `cob diff <coords>`                   | Is this published version intact?          |
| Local dir vs published     | `cob diff <dir> <coords>`             | Do my local files match what was published?|
| Version vs version         | `cob diff <coords-A> <coords-B>`      | What changed between these two releases?   |

**Manifest lint** -- schema + URI syntax + local-file existence. No AWS
calls. The same checks run implicitly at the top of every other
manifest-based command (`publish`, `promote`, `pull`, `diff --version`),
so a manifest that publishes cleanly will always lint cleanly first. This
mode is the user-facing report of that pre-flight.

```bash
cob diff my-package.yaml
```

Output labels each row: `(42.0 MB)` for verified-local,
`(remote, syntax only)` for unverified-remote. Safe in pre-commit / CI
lint stages without credentials.

**Manifest vs published** -- hashes each source and compares to the
published asset of the same name. Run before `publish --force` to see
exactly what would change. Source hash precedence, cheapest first:

1. a **known checksum** -- S3 object with `--checksum-algorithm SHA256`,
   a `ca://` source, or a local file (no download);
2. for an unchecksummed S3 source, the recorded **origin** (see Provenance):
   a `HeadObject` now, comparing `version_id`/`etag` to what was recorded
   at publish -- a no-download drift signal. Unchanged → recorded SHA is
   trusted (`match(origin)`); changed/unreadable → reported as drift;
3. otherwise the recorded **`cob-provenance.json`** SHA (`match(provenance)`);
4. with **`--deep`**, cob downloads and hashes the source (no S3 writes).

```bash
cob diff my-package.yaml --version 2.1.0
cob diff my-package.yaml --version 2.1.0 --deep   # hash unchecksummed sources
```

**Self-integrity** -- fetches the recorded `cob-provenance.json` for
`<coords>` and compares each entry's SHA-256 to what CodeArtifact
currently stores. The chain of evidence (who published/promoted it,
where each file came from, recursing through `ca://`) is printed first;
then the comparison. Audit a version you didn't build, with nothing but
its coordinates:

```bash
cob diff my-domain/dev/my-namespace/my-package@2.1.0
cob diff my-domain/dev/my-namespace/my-package@latest
```

**Local dir vs published** -- for each published asset of `<coords>`,
looks for a local file of the same name in `<dir>`, hashes it, and
compares to the published SHA-256. No manifest is consulted, no remote
URIs dereferenced. The straight answer to "do these local files match
what was published?"

```bash
# After `cob pull` (which writes ca:// pinned sources for re-publish
# lineage), the most direct check that your local copy is intact:
cob diff ~/pulled-dir my-domain/dev/my-namespace/my-package@2.1.0
```

A typical mismatch row:

```
  ✗ readability.jar                  mismatch
      local     195.5 MB   /home/me/pulled-dir/readability.jar              a3f2b8c9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1
      published 195.5 MB   my-domain/dev/my-namespace/my-package@2.1.3      25c4517cdef0123456789abcdef0123456789abcdef0123456789abcdef01234
```

`sha256sum` the local file and you can string-compare the full hash --
no truncation guesswork.

**Version vs version** -- compares two published versions of the same
package by the SHA-256 each side recorded in CodeArtifact. No downloads.
`cob-provenance.json` is excluded from both sides (its bytes trivially
differ on every publish/promote -- chain timestamps, IDs -- but that
isn't a package change). The two coordinates must reference the same
package; cross-repo same-package is allowed.

```bash
# What changed in this release?
cob diff acme/dev/tools/my-app@2.0.0 acme/dev/tools/my-app@2.1.0

# Did the promote preserve the bytes?
cob diff acme/dev/tools/my-app@2.1.0 acme/prod/tools/my-app@2.1.0
```

Flags: `--version` (manifest mode: `COB_VERSION` fallback), `--deep`
(manifest vs published only), `-v`/`--verbose`

### manifest

Prints a manifest (YAML, to stdout) for an existing version -- recover the
manifest for a package you have only coordinates for.

If the version has a `cob-provenance.json`, it is **reconstructed**
faithfully: original `sources:` keys, with each URI as it resolved at
publish. This is a *pinned snapshot* -- `${VERSION}`/`${env.*}` are already
expanded, and `promote.stages` is not recoverable (provenance records actual
promotes, not the declared list).

Otherwise it is **inferred**: the version has no provenance (non-cob or
pre-provenance), so real origins are unknown -- each asset is sourced from
the package itself via `ca://`. Re-publishing the inferred manifest
reproduces the same bytes. `cob-provenance.json` is never emitted as a
source.

```bash
cob manifest my-domain/dev/my-namespace/my-package@2.1.0 > my-package.yaml
cob manifest my-domain/dev/my-namespace/my-package@latest
```

Flags: `--version` (or use `@version` / `COB_VERSION`)

### Provenance

Every `cob publish` writes one extra asset, **`cob-provenance.json`** -- a
chain-of-evidence document, published last (it finalizes the version). It
records, with **no S3 writes**:

- **`chain`** -- an append-only event log. `publish` records the origin
  repo/version, time, region, the manifest's SHA-256, and the AWS
  principal (`account` / `arn` / `user_id`, from STS). `promote` does
  **not** copy the file verbatim -- it appends a `promote` link (from/to,
  time, actor), so the chain shows exactly who moved the version where.
- **`assets[].origin`** -- where each file physically came from at
  packaging time:
  - **s3**: `bucket`, `key`, `version_id`, `etag`, `last_modified`,
    `region` (the no-download drift signal used by `verify`/`diff`);
  - **ca**: the upstream coordinates, plus the upstream's *own*
    `cob-provenance.json` embedded recursively -- the full transitive
    history travels inside the package. An upstream not published by cob
    is recorded honestly as `upstream_status: no-cob-provenance` rather
    than failing; a re-check that finds it deleted reports `missing`
    (non-fatal -- the embedded evidence still stands).
  - **file**: `path` and `mtime`.

Because each upstream's provenance is already complete when cob reads it,
`ca://` embedding is one fetch per direct source and terminates naturally
(publish order is acyclic). The file grows with the dependency closure;
that's the intended trade-off for a self-contained evidence trail.

`promote` carries `assets` forward unchanged (same bytes) and only appends
to `chain`. The file appears in `cob ls <pkg>@ver` and is fetched by
`cob pull`; it is excluded from `diff`'s "not in manifest" reporting.

Read it with `cob diff <coordinates>` (the self-integrity mode): cob fetches
this document, compares each recorded SHA-256 to what CodeArtifact stores,
and prints the chain plus the recursive origin tree -- audit any cob-
published version with nothing but its coordinates. For chain-only viewing
without the byte comparison, use `cob log <coordinates>`.

### Parallel transfers

`publish`, `pull`, and `promote` transfer assets in parallel, bounded by
`--concurrency` (default 4; `1` = sequential, the old behaviour). For
`publish`/`promote` the `cob-provenance.json` write is the finalizer and is
always sequenced last, after every other asset has uploaded, so a failure
leaves the version Unfinished exactly as before.

## Source types

| Prefix | Description | Example |
|--------|-------------|---------|
| `s3://` | S3 object | `s3://bucket/path/to/file.tar.gz` |
| `ca://` | CodeArtifact asset | `ca://domain/repo/ns/pkg@version/asset` |
| `./` or path | Local file | `./local-config.yaml` |

Relative paths resolve from the manifest file's directory, not the working directory. Bare filenames (without `./` prefix) are also treated as relative paths.

A manifest is executable configuration -- like a Makefile. `publish` and `validate` read whatever local files it names, so don't run a manifest you don't trust.

`@latest` is not supported in `ca://` source URIs. Use `${VERSION}` instead.

### Asset names

The YAML key in `sources:` is only a label -- for the manifest and logs. The
asset name stored in CodeArtifact is the source's basename:

| Source | Stored asset name |
|--------|-------------------|
| `s3://bucket/builds/app-2.1.0.tar.gz` | `app-2.1.0.tar.gz` |
| `ca://acme/dev/tools/shared-lib@2.0.0/shared-lib-2.0.deb` | `shared-lib-2.0.deb` |
| `./local-overrides.yaml` | `local-overrides.yaml` |

`cob pull <coords> <asset>` and `--assets` select by this stored name, not by
the manifest key. Put `${VERSION}` in the source URI so the version travels
with the filename (`app-${VERSION}.tar.gz`).

Because the basename is the identity, two sources that resolve to the same
basename are rejected -- one would silently overwrite the other in
CodeArtifact. `validate` reports this offline.

### S3 checksums

S3 objects uploaded with `--checksum-algorithm SHA256` store the hash in metadata; cob cross-checks it against the SHA-256 it computes while streaming and rejects the asset if they disagree. There is no size limit -- assets are streamed through a temp file, not held in memory (see Known limitations).

```bash
aws s3 cp file.tar.gz s3://bucket/key --checksum-algorithm SHA256
```

cob resolves an S3 bucket's region automatically, so `--region` need not
match the bucket's region for `s3://` sources.

## Variable substitution

Source URIs support two variable namespaces:

- `${VERSION}` -- from `--version` flag or `COB_VERSION` env var
- `${env.NAME}` -- reads the environment variable `COB_VAR_NAME`

`${env.NAME}` deliberately does **not** read an arbitrary variable named
`NAME`. It reads `COB_VAR_` + `NAME`, so `${env.GIT_SHA}` resolves from
`COB_VAR_GIT_SHA`. This namespacing keeps a manifest from pulling a secret
like `AWS_SECRET_ACCESS_KEY` into a source URI -- a resolved URI is recorded
in published provenance and sent to the URI's host.

Unresolved variables are a hard error.

```yaml
sources:
  release: s3://my-bucket/builds/app-${VERSION}.tar.gz
  config:  s3://my-bucket/builds/${env.GIT_SHA}/config.yaml
```

```bash
COB_VAR_GIT_SHA=abc123 cob publish my-package.yaml --version 2.1.0
```

## Global flags

```
--profile      AWS profile
--region       AWS region
--json         Machine-readable JSON output
--quiet, -q    Suppress headers, summaries, and progress (errors still print)
--debug        Log AWS API responses/retries to stderr
--tmpdir       Directory for streaming spill files (default: $TMPDIR)
--no-tui       Force line-stream output even on a TTY (also: COB_TUI=0)
```

On an interactive terminal, `publish` / `pull` / `promote` render a live
multi-row progress view -- one row per asset, in-place updates, progress
bar / rate / ETA per row, totals at the bottom. The view scrolls into
shell history when the command exits (no alt-screen takeover). Output
piped to a file, JSON mode, and `--quiet` automatically fall back to the
stream renderer (one `OK <name>` line per completed asset, no in-place
updates). `--no-tui` (or `COB_TUI=0`) forces stream output even on a
TTY -- useful for screen recording, exotic terminal emulators, or
copy-paste-friendly logs.

`--debug` is the first thing to reach for when an AWS call fails for a
non-obvious reason (region, credentials, throttling) -- it logs every AWS
response status line and retry attempt to stderr without touching stdout.
Request logging is deliberately omitted: a signed AWS request header carries
a live session credential (`X-Amz-Security-Token`), and `--debug` output
often ends up in CI logs.

## Authentication

Tries in order:

1. `--profile` flag or `COB_PROFILE`
2. `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY`
3. Default credential chain (instance roles, ECS task roles)

No automatic SSO login. If an SSO token is expired, cob tells you to run `aws sso login`.

## Environment variables

All `COB_*` variables sit in the middle of the precedence chain: **CLI flags > env vars > manifest file**.

```
COB_VERSION      Package version (--version fallback, ${VERSION} in source URIs)
COB_DOMAIN       Override manifest domain
COB_REPOSITORY   Override manifest repository
COB_NAMESPACE    Override manifest namespace
COB_PACKAGE      Override manifest package
COB_PROFILE      AWS profile (--profile fallback)
COB_REGION       AWS region (--region fallback)
COB_TMPDIR       Spill directory (--tmpdir fallback)
COB_JSON         Set 1/true to default to --json output
COB_QUIET        Set 1/true to default to --quiet output
COB_DEBUG        Set 1/true to default to --debug logging
COB_TUI          Set 0 to disable the live progress view (same as --no-tui)
ACCESSIBLE       Set 1 to force the line-stream renderer (screen-reader friendly)
COB_VAR_*        Values for ${env.*} in source URIs (see Variable substitution)
```

`ACCESSIBLE` is a cross-tool convention (Charm libs, `gh`, others) for
"I'm using a screen reader; please degrade to plain text." cob honors it
identically to `--no-tui` / `COB_TUI=0` -- the live TUI's box-drawing and
cursor-positioning escapes are unusable in that mode.

Standard AWS environment variables (`AWS_REGION`, `AWS_PROFILE`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`) are also respected through the default credential chain.

This means a single manifest can target different domains/repos in CI without editing the file:

```bash
COB_DOMAIN=acme-prod COB_REPOSITORY=prod cob publish my-package.yaml --version 1.0.0
```

When an env var overrides a manifest field, cob prints a warning so you know it's active:

```
Warning: using COB_DOMAIN=acme-prod (overrides manifest domain)
```

A few things to know about scope:

- **Manifest overrides only apply to manifest-based operations.** Compact coordinates (`cob pull mydom/repo/ns/pkg@1.0.0`) are explicit and ignore `COB_DOMAIN` and friends entirely.
- **`COB_REPOSITORY` does not affect promote's source inference.** Promote walks the `promote.stages` list to determine the source repo. Setting `COB_REPOSITORY` changes where publish targets, but promote still moves between defined stages. This is intentional -- promote's purpose is moving between stages, not targeting an arbitrary repo.

## JSON output

Most commands support `--json` for machine-readable output. JSON goes to stdout, errors always go to stderr. A JSON object is emitted even on early failures (auth, config) so CI pipelines can reliably parse the output.

`publish`, `pull`, `promote`, `verify`, `diff`, `rm` emit a `CommandResult` object with `command`, `package`, `repository`, `region`, `actor`, `assets`, `status`, etc. `region` and `actor` (STS account/ARN/user_id) identify which account/role and region executed the operation -- audit pipelines no longer need to scan per-asset Origin records or the provenance chain to attribute a run. Any warnings raised during the command are collected into its `warnings` array -- warnings also print to stderr, but `--json` consumers should read this field.

`ls` emits an array of the relevant type: packages, versions, assets, or promotion statuses.

`ls -R` emits a flat array of strings (one fully-qualified coordinate per leaf), for shell pipelines.

`tree` emits a flat array of typed records -- one per walked node, in walk order, no nesting. Each record carries `path`, `kind` (`domain`|`repo`|`package`|`version`|`asset`), the matching coordinate components, and a typed payload (`repo_count`, `package_count`, `version_count`/`latest_version`, `asset_count`/`published`, `size`/`sha256`) -- jq filters on `select(.kind == "package" and .version_count > 5)` work directly, without recursion or parsing stringy metadata.

`log` emits the full `Provenance` struct: schema, package, `chain[]` of events (publish/promote), `assets[]` with per-asset SHA + `origin` (recursing into `ca://` upstreams).

`resolve` emits its own minimal schema designed for scripting:

```json
{"package": "ns/pkg", "repository": "domain/repo", "version": "2.1.0"}
```

One exception: `manifest` always prints a YAML manifest (that *is* its output -- `--json` does not apply). Every other command honours `--json`.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Error -- the command could not be completed (auth, config, network) |
| 2 | Not found (package/version/asset doesn't exist) |
| 3 | Conflict (version exists, use `--force`) |
| 4 | Verification failed -- the check ran and found a difference (`verify` SHA mismatch, `diff` drift) |
| 130 | Interrupted -- user hit Ctrl-C; in-flight transfers were aborted |

Code 4 is the one that matters for `verify`/`diff` as CI gates: it means the
published bytes genuinely differ from what was expected, as opposed to code 1
which means the check itself could not run. Both are non-zero, so either still
blocks a pipeline.

Code 130 follows the POSIX shell convention (128 + SIGINT). Distinguishes a
deliberate cancellation from a transient failure, so CI retry policies can
choose whether to honor it. `publish` and `promote` leave the version
**Unfinished** after a 130 -- re-run with `--resume` to continue from where
the cancel landed.

## Package composition patterns

### Shared assets

If the same artifact appears in multiple packages, extract it into its own package and reference it with `ca://` URIs.

Instead of four manifests each pulling the same file from S3:

```yaml
# bad -- same S3 source repeated in 4 manifests, 4 copies in CodeArtifact
sources:
  shared-lib: s3://artifacts/shared-lib-2.0.deb
```

Create a dedicated package:

```bash
# tools/shared-lib@2.0.0 has one asset: shared-lib-2.0.deb
cob publish shared-lib.yaml --version 2.0.0
```

Then reference it from consuming packages:

```yaml
# my-app.yaml
sources:
  shared-lib: ca://acme/dev/tools/shared-lib@2.0.0/shared-lib-2.0.deb
  app:        s3://artifacts/my-app-${VERSION}.tar.gz
```

One source of truth, one place to update when the version changes.

### Pin versions in `ca://` URIs

Always pin to a specific version in `ca://` source URIs. `@latest` is not supported (and is explicitly rejected), and `${VERSION}` expands to the version of the package you're *publishing*, not the version of the dependency you're pulling from.

If you need the dependency version to vary per environment, use an env var:

```yaml
sources:
  shared-lib: ca://acme/dev/tools/shared-lib@${env.LIB_VERSION}/shared-lib.deb
```

But a pinned version is usually better -- it makes builds reproducible. Same manifest, same output, every time.

### Promotion ordering

When a consuming package references a `ca://` source in a specific repository, that source must already exist there. If `my-app` references `ca://acme/staging/tools/shared-lib@2.0.0/...`, then `tools/shared-lib@2.0.0` must be promoted to staging before `my-app` can be published or promoted to staging.

In practice this means your CI pipeline should promote dependencies before dependents:

```bash
# Promote the shared package first
cob promote acme/dev/tools/shared-lib@2.0.0 --to staging

# Then promote the consuming package
cob promote acme/dev/apps/my-app@1.5.0 --to staging
```

If your `ca://` URIs use a variable for the repository (`ca://acme/${env.TARGET_REPO}/...`), set it at publish time so each stage's package points to its own repo.

### Naming shared packages

Name packages for what they are, not for the fact that they're shared. `tools/shared-lib` is clear. `common/misc-stuff` becomes a junk drawer. If assets aren't related to each other, they belong in separate packages even if multiple consumers reference them.

## Known limitations

- **`--force` is not atomic.** Deletes the existing version then re-publishes. Brief window where the version doesn't exist.
- **Transfers spill to a temp file, not memory.** CodeArtifact's API requires an `io.ReadSeeker` (Content-Length + retries), so true end-to-end streaming isn't possible; cob streams each asset through a temp file in `$TMPDIR` instead of buffering in RAM. Memory stays bounded and there is no asset size limit, but a publish/promote needs free temp disk for the largest single asset. Note that `$TMPDIR` is `tmpfs` (RAM-backed) on many Linux systems -- for large assets, point `--tmpdir`/`COB_TMPDIR` at real disk with room for `concurrency` × the largest asset.
- **`@latest` resolves by timestamp, not semver.** The most recently published version wins, regardless of version string ordering.

## License

MIT -- see [LICENSE](LICENSE).
