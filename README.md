# cob

Assemble AWS CodeArtifact packages from remote sources. No local artifacts required.

cob reads a YAML manifest that declares what goes into a package by pointing at S3 objects, other CodeArtifact packages, or local files. Assets flow from source to CodeArtifact through memory -- they never touch disk.

## Install

```bash
go install github.com/jmurray2011/cob/cmd/cob@latest
```

Or build from source:

```bash
go build -o cob ./cmd/cob
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

Flags: `--version`, `--dry-run`, `--force`, `--yes`

### pull

Downloads assets to a local directory. Works with a manifest or compact coordinates.

```bash
# With a manifest -- pull all assets
cob pull my-package.yaml --version 2.1.0 --output ./assets/

# Ad-hoc -- grab one asset by name
cob pull my-domain/dev/my-namespace/my-package@2.1.0 app

# Ad-hoc -- grab all assets
cob pull my-domain/dev/my-namespace/my-package@2.1.0 --output ./assets/

# Pull the latest published version
cob pull my-domain/dev/my-namespace/my-package@latest --output ./assets/

# Pull specific assets by name
cob pull my-domain/dev/my-namespace/my-package@2.1.0 --assets app,config
```

Skips files that already exist with a matching SHA-256.

Flags: `--version`, `--output`, `--assets`

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
```

Flags: `--to` (required), `--version`, `--force`, `--yes`

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

## Source types

| Prefix | Description | Example |
|--------|-------------|---------|
| `s3://` | S3 object | `s3://bucket/path/to/file.tar.gz` |
| `ca://` | CodeArtifact asset | `ca://domain/repo/ns/pkg@version/asset` |
| `./` or path | Local file | `./local-config.yaml` |

Relative paths resolve from the manifest file's directory, not the working directory. Bare filenames (without `./` prefix) are also treated as relative paths.

`@latest` is not supported in `ca://` source URIs. Use `${VERSION}` instead.

### S3 checksums

S3 objects uploaded with `--checksum-algorithm SHA256` store the hash in metadata, letting cob skip re-hashing the buffer. Objects without a checksum are hashed in memory before publishing. Objects over 512 MB without a checksum are rejected.

```bash
aws s3 cp file.tar.gz s3://bucket/key --checksum-algorithm SHA256
```

## Variable substitution

Source URIs support two variable namespaces:

- `${VERSION}` -- from `--version` flag or `COB_VERSION` env var
- `${env.WHATEVER}` -- reads from environment variables

Unresolved variables are a hard error.

```yaml
sources:
  release: s3://my-bucket/builds/app-${VERSION}.tar.gz
  config:  s3://my-bucket/builds/${env.GIT_SHA}/config.yaml
```

## Global flags

```
--profile    AWS profile
--region     AWS region
--json       Machine-readable JSON output
```

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

All commands support `--json` for machine-readable output. JSON goes to stdout, errors always go to stderr. A JSON object is emitted even on early failures (auth, config) so CI pipelines can reliably parse the output.

`publish`, `pull`, `promote` emit a `CommandResult` object with `command`, `package`, `repository`, `assets`, `status`, etc.

`ls` emits an array of the relevant type: packages, versions, assets, or promotion statuses.

`resolve` emits its own minimal schema designed for scripting:

```json
{"package": "ns/pkg", "repository": "domain/repo", "version": "2.1.0"}
```

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Error (auth, config, network) |
| 2 | Not found (package/version/asset doesn't exist) |
| 3 | Conflict (version exists, use `--force`) |

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
- **All transfers buffer in memory.** CodeArtifact's API requires an `io.ReadSeeker`, so true streaming isn't possible.
- **`@latest` resolves by timestamp, not semver.** The most recently published version wins, regardless of version string ordering.
