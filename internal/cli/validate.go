package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func newValidateCmd() *cobra.Command {
	var flagVersion string

	cmd := &cobra.Command{
		Use:   "validate <manifest>",
		Short: "Check a manifest offline (no AWS calls)",
		Long: "Validates manifest schema, variable resolvability, and source URI " +
			"syntax without contacting AWS. Local file sources are checked for " +
			"existence. Designed for pre-commit and CI lint stages.",
		Example: `  cob validate ./my-package.yaml
  cob validate ./my-package.yaml --version 2.1.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidate(args[0], flagVersion)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Version to resolve ${VERSION} with (optional)")
	return cmd
}

func runValidate(manifestPath, versionFlag string) error {
	out := newWriter(flagJSON)

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

	var failures int
	byAsset := make(map[string]string, len(m.Sources)) // stored name -> manifest key
	for _, s := range m.Sources {
		ar := cob.AssetResult{Name: s.Name, Source: s.URI, Method: "ok"}

		resolved, err := manifest.ExpandURI(s.URI, version)
		if err != nil {
			ar.SetError(err)
			out.AssetFail(s.Name, s.URI, err)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Source = resolved

		asset, size, err := validateSourceURI(resolved, m.Dir)
		if err != nil {
			ar.SetError(err)
			out.AssetFail(s.Name, resolved, err)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Size = size
		if prev, dup := byAsset[asset]; dup {
			e := fmt.Errorf("collides with source %q: both publish as asset %q", prev, asset)
			ar.SetError(e)
			out.AssetFail(s.Name, resolved, e)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		byAsset[asset] = s.Name

		out.AssetOK(&ar, resolved)
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
	out.Summary("Valid: %d sources, %d-stage promote pipeline.", len(m.Sources), promoteStageCount(m))
	return out.CommandResult(result)
}

// validateSourceURI checks a (variable-resolved) source URI's syntax without
// any network call and returns the stored asset name (basename) it would
// publish as, plus its size. Local files are additionally checked for
// existence and report their real size; remote sources report 0 because
// sizing them would need a network call validate deliberately avoids. It
// shares classifyURI with publish's buildSource, so the two commands agree
// on which URIs are valid.
func validateSourceURI(uri, manifestDir string) (string, int64, error) {
	kind, path, err := classifyURI(uri, manifestDir)
	if err != nil {
		return "", 0, err
	}
	switch kind {
	case uriS3:
		s, err := cob.NewS3Source(nil, uri)
		if err != nil {
			return "", 0, err
		}
		return s.Filename(), 0, nil
	case uriCA:
		s, err := cob.NewCASource(nil, uri)
		if err != nil {
			return "", 0, err
		}
		return s.Filename(), 0, nil
	default: // uriFile
		info, err := os.Stat(path)
		if err != nil {
			return "", 0, fmt.Errorf("local source not found: %s", path)
		}
		if info.IsDir() {
			return "", 0, fmt.Errorf("local source is a directory: %s", path)
		}
		return filepath.Base(path), info.Size(), nil
	}
}

func promoteStageCount(m *manifest.Manifest) int {
	if m.Promote == nil {
		return 0
	}
	return len(m.Promote.Stages)
}
