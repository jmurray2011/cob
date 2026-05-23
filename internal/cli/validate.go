package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

func newValidateCmd(cfg *Config) *cobra.Command {
	var flagVersion string

	cmd := &cobra.Command{
		Use:   "validate <manifest>",
		Short: "Check a manifest offline (no AWS calls)",
		Long: "Validates manifest schema, variable resolvability, and source URI " +
			"syntax — no AWS calls. Local file sources are checked for " +
			"existence and that they're regular files; remote sources " +
			"(s3:// and ca://) are syntax-only — validate never " +
			"dereferences them. For end-to-end byte verification against a " +
			"published version, use `cob verify`. Designed for pre-commit " +
			"and CI lint stages.",
		Example: `  cob validate ./my-package.yaml
  cob validate ./my-package.yaml --version 2.1.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidate(cfg, args[0], flagVersion)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Version to resolve ${VERSION} with (optional)")
	return cmd
}

func runValidate(cfg *Config, manifestPath, versionFlag string) error {
	out := newWriter(cfg)
	defer out.Close()

	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "validate", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)

	// A real version is used when supplied (--version / COB_VERSION); otherwise
	// a placeholder keeps ${VERSION} from failing structural checks while still
	// catching unset ${env.*} and unknown variables.
	version := versionFlag
	if version == "" {
		version = os.Getenv("COB_VERSION")
	}
	versionResolved := version != ""
	if !versionResolved {
		version = "0.0.0-validate"
	}

	out.Header("Validating %s", manifestPath)

	result := &cob.CommandResult{
		Command:    "validate",
		Package:    fmt.Sprintf("%s/%s", m.Namespace, m.Package),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}

	var failures, localOK, remoteOK int
	byAsset := make(map[string]string, len(m.Sources)) // stored name -> manifest key
	for _, s := range m.Sources {
		ar := cob.AssetResult{Name: s.Name, Source: s.URI, Method: "syntax"}

		resolved, err := manifest.ExpandURI(s.URI, version)
		if err != nil {
			ar.SetError(err)
			out.Plain("  ✗ %s  %s", s.Name, err)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Source = resolved

		asset, size, kind, err := validateSourceURI(resolved, m.Dir)
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

		// Distinct rendering per kind so the user can see *what* validate
		// actually checked: a local file got its existence + size verified;
		// a remote URI got nothing but a syntax pass. The current line was
		// previously rendered through the asset-stream pipeline as
		// "0 B 0ms ok" — meaningless numbers that implied measurement
		// where none had happened.
		switch kind {
		case uriFile:
			ar.Method = "exists"
			localOK++
			out.Plain("  ✓ %s  %s  (%s)", s.Name, resolved, output.FormatSize(size))
		case uriS3:
			ar.Method = "syntax(s3)"
			remoteOK++
			out.Plain("  ✓ %s  %s  (remote, syntax only)", s.Name, resolved)
		case uriCA:
			ar.Method = "syntax(ca)"
			remoteOK++
			out.Plain("  ✓ %s  %s  (remote, syntax only)", s.Name, resolved)
		}
		result.Assets = append(result.Assets, ar)
	}

	if !versionResolved {
		out.Warn("no --version/COB_VERSION; ${VERSION} validated structurally only")
	}

	if failures > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d of %d sources invalid", failures, len(m.Sources))
		out.Summary("Invalid: %d of %d sources failed.", failures, len(m.Sources))
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}

	// Spell out the contract: schema + URI syntax pass, plus what was
	// actually verified vs not. A user who deleted the asset files but
	// has a manifest of remote URIs should see why validate didn't flag
	// it — and what command to use instead.
	switch {
	case localOK > 0 && remoteOK > 0:
		out.Summary("Valid: %d sources schema-OK — %d local files verified to exist, %d remote URIs syntax-only (use `cob verify` for remote integrity). %d-stage promote pipeline.",
			len(m.Sources), localOK, remoteOK, promoteStageCount(m))
	case localOK > 0:
		out.Summary("Valid: %d sources schema-OK — %d local files verified to exist. %d-stage promote pipeline.",
			len(m.Sources), localOK, promoteStageCount(m))
	case remoteOK > 0:
		out.Summary("Valid: %d sources schema-OK — all remote URIs, syntax-only (use `cob verify` against a published version for byte integrity). %d-stage promote pipeline.",
			len(m.Sources), promoteStageCount(m))
	default:
		out.Summary("Valid: %d sources, %d-stage promote pipeline.", len(m.Sources), promoteStageCount(m))
	}
	return out.CommandResult(result)
}

// validateSourceURI checks a (variable-resolved) source URI's syntax without
// any network call and returns the stored asset name (basename) it would
// publish as, its size, and the kind so the renderer can describe exactly
// what was verified (local existence vs remote syntax-only). For local
// files, size is the real on-disk size and existence is asserted; for
// remote sources, size is 0 — sizing them would need a network call that
// validate deliberately avoids. Shares classifyURI with publish's
// buildSource, so the two commands agree on which URIs are valid.
func validateSourceURI(uri, manifestDir string) (asset string, size int64, kind uriKind, err error) {
	kind, path, err := classifyURI(uri, manifestDir)
	if err != nil {
		return "", 0, kind, err
	}
	switch kind {
	case uriS3:
		s, sErr := cob.NewS3Source(nil, uri)
		if sErr != nil {
			return "", 0, kind, sErr
		}
		return s.Filename(), 0, kind, nil
	case uriCA:
		s, cErr := cob.NewCASource(nil, uri)
		if cErr != nil {
			return "", 0, kind, cErr
		}
		return s.Filename(), 0, kind, nil
	default: // uriFile
		info, sErr := os.Stat(path)
		if sErr != nil {
			return "", 0, kind, fmt.Errorf("local source not found: %s", path)
		}
		if !info.Mode().IsRegular() {
			// FIFOs, sockets, device nodes etc. would hang the eventual
			// publish; fail at validate time instead.
			return "", 0, kind, fmt.Errorf("local source is not a regular file: %s (mode %s)", path, info.Mode())
		}
		return filepath.Base(path), info.Size(), kind, nil
	}
}

func promoteStageCount(m *manifest.Manifest) int {
	if m.Promote == nil {
		return 0
	}
	return len(m.Promote.Stages)
}
