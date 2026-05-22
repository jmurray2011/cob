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
cob publish my-package.yaml --version 2.1.0 --yes        # skip confirmation prompt
COB_VERSION=2.1.0 cob publish my-package.yaml            # version from env
```

Flags: `--version`, `--dry-run`, `--force`, `--yes`, `--concurrency`

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
```

Flags: `--to` (required), `--version`, `--force`, `--yes`, `--dry-run`, `--concurrency`

### ls

Drill into CodeArtifact at any level:

```bash
cob ls my-domain/dev                                            # list packages
cob ls my-domain/dev/my-namespace/my-package                    # list versions
cob ls my-domain/dev/my-namespace/my-package@2.1.0              # list assets
cob ls my-domain/*/my-namespace/my-package@2.1.0                # promotion status across repos
cob ls my-domain/dev/my-namespace/my-package@latest             # assets in latest version
cob ls my-domain/dev --all-repos                                # shorthand for wildcard repo
```

Flags: `--all-repos`

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

### validate

Checks a manifest **offline** -- no AWS calls. Schema, variable
resolvability, source-URI syntax, and local-file existence. Good for
pre-commit / CI lint stages.

```bash
cob validate my-package.yaml
cob validate my-package.yaml --version 2.1.0   # also resolves ${VERSION}
```

Flags: `--version` (optional)

### verify

Checks a published version's integrity. No mutation; exits non-zero on any
mismatch. Takes a manifest **or** compact coordinates.

**Coordinates (no manifest)** -- self-verifies a version against its own
recorded `cob-provenance.json`: every recorded asset must still hash to what
was recorded, and the chain of evidence (who published/promoted it, where
each file came from, recursing through `ca://`) is printed. Audit a version
you didn't build, with nothing but its coordinates:

```bash
cob verify my-domain/dev/my-namespace/my-package@2.1.0
cob verify my-domain/dev/my-namespace/my-package@latest
```

**Manifest** -- compares each manifest source's SHA-256 against the
published assets. A CI gate for reproducible builds. Each source is checked
by this precedence, cheapest first:

1. a **known checksum** -- S3 object with `--checksum-algorithm SHA256`,
   a `ca://` source, or a local file (no download);
2. for an unchecksummed S3 source, the recorded **origin** (see Provenance):
   a `HeadObject` now, comparing `version_id`/`etag` to what was recorded
   at publish -- a no-download drift signal. Unchanged → the recorded SHA
   is trusted (`match(origin)`); changed/unreadable → reported as drift;
3. otherwise the recorded **`cob-provenance.json`** SHA (`match(provenance)`);
4. with **`--deep`**, cob downloads and hashes the source (no S3 writes).

Only sources with none of the above are reported *unverified*. The match
line shows the basis, e.g. `match(source)`, `match(origin)`,
`match(provenance)`, `match(deep)`.

```bash
cob verify my-package.yaml --version 2.1.0
cob verify my-package.yaml --version 2.1.0 --deep   # download+hash unchecksummed sources
```

Flags: `--version` (required, or `COB_VERSION`), `--deep` (manifest mode only)

### diff

Shows how the manifest differs from a published version: added (`+`),
removed (`-`), changed (`~`). Uses the same precedence as `verify`
(known checksum → recorded S3 origin → provenance → `--deep`); a changed
S3 `etag`/`version_id` shows as `~` with no download. Exits `1` on any
drift, `0` when identical -- like `diff(1)`. Run before `publish --force`
to see exactly what would change.

```bash
cob diff my-package.yaml --version 2.1.0
cob diff my-package.yaml --version 2.1.0 --deep
```

Flags: `--version` (required, or `COB_VERSION`), `--deep`

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
`cob pull`; it is excluded from `verify`'s "not in manifest" reporting.

Read it with `cob verify <coordinates>` (no manifest): it self-verifies the
version against this document and prints the chain plus the recursive origin
tree -- audit any cob-published version with nothing but its coordinates.

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
--profile    AWS profile
--region     AWS region
--json       Machine-readable JSON output
--debug      Log AWS API responses/retries to stderr
```

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
COB_VAR_*        Values for ${env.*} in source URIs (see Variable substitution)
```

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

`publish`, `pull`, `promote` emit a `CommandResult` object with `command`, `package`, `repository`, `assets`, `status`, etc. Any warnings raised during the command are collected into its `warnings` array -- warnings also print to stderr, but `--json` consumers should read this field.

`ls` emits an array of the relevant type: packages, versions, assets, or promotion statuses.

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

Code 4 is the one that matters for `verify`/`diff` as CI gates: it means the
published bytes genuinely differ from what was expected, as opposed to code 1
which means the check itself could not run. Both are non-zero, so either still
blocks a pipeline.

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
- **No resume on partial failure.** `--force` re-publishes all assets.
- **Transfers spill to a temp file, not memory.** CodeArtifact's API requires an `io.ReadSeeker` (Content-Length + retries), so true end-to-end streaming isn't possible; cob streams each asset through a temp file in `$TMPDIR` instead of buffering in RAM. Memory stays bounded and there is no asset size limit, but a publish/promote needs free temp disk for the largest single asset.
- **`@latest` resolves by timestamp, not semver.** The most recently published version wins, regardless of version string ordering.

## License

MIT -- see [LICENSE](LICENSE).
