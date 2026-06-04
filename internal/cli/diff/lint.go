package diff

import (
	"fmt"
	"os"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"

	"github.com/jmurray2011/cob/internal/cliutil"
)

// runLint runs the offline checks the old `cob validate` exposed.
// Same per-source rendering — labels each line with what was actually
// checked (local existence vs remote syntax-only). The underlying
// cliutil.ValidateManifest helper is the same one publish/promote/pull invoke
// implicitly, so "lint says it's OK" and "implicit pre-flight said
// nothing" can never disagree.
func runLint(out *output.Writer, manifestPath string) error {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return cliutil.Fail(out, "diff", cob.ExitError, "%s", err)
	}
	cliutil.WarnManifestOverrides(m, out)

	version := os.Getenv("COB_VERSION")
	versionResolved := version != ""
	if !versionResolved {
		version = "0.0.0-validate"
	}

	out.Header("Linting %s", manifestPath)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s", m.Namespace, m.Package),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}

	var failures, localOK, remoteOK int
	byAsset := make(map[string]string, len(m.Sources))
	for _, s := range m.Sources {
		ar := cob.AssetResult{Name: s.Name, Kind: cob.KindLint, Source: s.URI, Method: cob.LintSyntax}

		resolved, err := manifest.ExpandURI(s.URI, version)
		if err != nil {
			ar.SetError(err)
			out.Plain("  ✗ %s  %s", s.Name, err)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Source = resolved

		asset, size, kind, err := cliutil.ValidateSourceURI(resolved, m.Dir)
		if err != nil {
			ar.SetError(err)
			out.Plain("  ✗ %s  %s", s.Name, err)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Size = size
		if asset == cob.ProvenanceFile {
			e := fmt.Errorf("uses the reserved asset name %q (cob writes that as the publish finalizer)", asset)
			ar.SetError(e)
			out.Plain("  ✗ %s  %s", s.Name, e)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		if prev, dup := byAsset[asset]; dup {
			e := fmt.Errorf("collides with source %q: both publish as asset %q", prev, asset)
			ar.SetError(e)
			out.Plain("  ✗ %s  %s", s.Name, e)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		byAsset[asset] = s.Name

		switch kind {
		case cliutil.URIFile:
			ar.Method = cob.LintExists
			localOK++
			out.Plain("  ✓ %s  %s  (%s)", s.Name, resolved, output.FormatSize(size))
		case cliutil.URIS3:
			ar.Method = cob.LintSyntaxS3
			remoteOK++
			out.Plain("  ✓ %s  %s  (remote, syntax only)", s.Name, resolved)
		case cliutil.URICA:
			ar.Method = cob.LintSyntaxCA
			remoteOK++
			out.Plain("  ✓ %s  %s  (remote, syntax only)", s.Name, resolved)
		}
		result.Assets = append(result.Assets, ar)
	}

	if !versionResolved {
		out.Warn("no COB_VERSION; ${VERSION} validated structurally only")
	}

	if failures > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d of %d sources invalid", failures, len(m.Sources))
		out.Summary("Invalid: %d of %d sources failed.", failures, len(m.Sources))
		out.CommandResult(result)
		return &cliutil.ExitError{Code: cob.ExitError}
	}

	switch {
	case localOK > 0 && remoteOK > 0:
		out.Summary("Lint OK: %d sources — %d local files verified to exist, %d remote URIs syntax-only. %d-stage promote pipeline.",
			len(m.Sources), localOK, remoteOK, cliutil.PromoteStageCount(m))
	case localOK > 0:
		out.Summary("Lint OK: %d sources — %d local files verified to exist. %d-stage promote pipeline.",
			len(m.Sources), localOK, cliutil.PromoteStageCount(m))
	case remoteOK > 0:
		out.Summary("Lint OK: %d sources — all remote URIs, syntax-only (pass --version to diff bytes against a published version). %d-stage promote pipeline.",
			len(m.Sources), cliutil.PromoteStageCount(m))
	default:
		out.Summary("Lint OK: %d sources, %d-stage promote pipeline.", len(m.Sources), cliutil.PromoteStageCount(m))
	}
	return out.CommandResult(result)
}
