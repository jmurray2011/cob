package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

// validateManifest runs offline schema / URI-syntax / local-file-
// existence checks against a loaded manifest. Returns a single combined
// error listing every problem (one per line). Called silently at the
// top of every manifest-consuming command (publish, promote, pull
// manifest mode, diff manifest mode) — so a broken manifest fails fast
// before any AWS work. The companion runDiffLint exposes the same
// checks as a user-visible report; both share validateSourceURI so
// "implicit validation said it's OK" and "lint says it's OK" never
// disagree.
//
// version is what ${VERSION} expands to during URI syntax checks. Pass
// the real version if known; pass "" to use a placeholder that just
// keeps ${VERSION} from breaking schema checks while still catching
// unset ${env.*} and unknown variables.
func validateManifest(m *manifest.Manifest, version string) error {
	if version == "" {
		version = "0.0.0-validate"
	}
	var errs []string
	byAsset := make(map[string]string, len(m.Sources)) // asset name → first source that produced it
	for _, s := range m.Sources {
		resolved, err := manifest.ExpandURI(s.URI, version)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", s.Name, err))
			continue
		}
		asset, _, _, err := validateSourceURI(resolved, m.Dir)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", s.Name, err))
			continue
		}
		if asset == cob.ProvenanceFile {
			errs = append(errs, fmt.Sprintf("%s: uses the reserved asset name %q (cob writes that as the publish finalizer)", s.Name, asset))
			continue
		}
		if prev, dup := byAsset[asset]; dup {
			errs = append(errs, fmt.Sprintf("%s: collides with source %q — both publish as asset %q", s.Name, prev, asset))
			continue
		}
		byAsset[asset] = s.Name
	}
	if len(errs) > 0 {
		return fmt.Errorf("manifest validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateSourceURI checks a (variable-resolved) source URI's syntax
// without any network call and returns the stored asset name (basename)
// it would publish as, its size, and the kind so callers can describe
// exactly what was verified (local existence vs remote syntax-only).
// For local files, size is the real on-disk size and existence is
// asserted; for remote sources, size is 0 — sizing them would need a
// network call validation deliberately avoids. Shares classifyURI with
// publish's buildSource, so the two paths agree on which URIs are
// valid.
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
			// publish; fail at validation time instead.
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
