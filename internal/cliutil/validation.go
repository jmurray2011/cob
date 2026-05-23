package cliutil

import (
	"fmt"
	"strings"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

// ValidateManifest runs offline schema / URI-syntax / local-file-
// existence checks against a loaded manifest. Returns a single combined
// error listing every problem (one per line). Called silently at the
// top of every manifest-consuming command (publish, promote, pull
// manifest mode, diff manifest mode) — so a broken manifest fails fast
// before any AWS work. The companion diff-lint exposes the same checks
// as a user-visible report; both share ValidateSourceURI so "implicit
// validation said it's OK" and "lint says it's OK" never disagree.
//
// version is what ${VERSION} expands to during URI syntax checks. Pass
// the real version if known; pass "" to use a placeholder that just
// keeps ${VERSION} from breaking schema checks while still catching
// unset ${env.*} and unknown variables.
func ValidateManifest(m *manifest.Manifest, version string) error {
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
		asset, _, _, err := ValidateSourceURI(resolved, m.Dir)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", s.Name, err))
			continue
		}
		if err := RegisterAssetName(byAsset, asset, s.Name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", s.Name, err))
			continue
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("manifest validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// RegisterAssetName performs the two asset-name checks both BuildSources
// and ValidateManifest need: reject the reserved cob-provenance.json name
// (cob writes that as the publish finalizer) and reject any collision
// with a name already recorded in byAsset. On success, records
// (name → srcKey) so subsequent checks see the new entry.
//
// Callers prepend their own per-source label (manifest key) when wrapping
// the returned error so the two messages stay where the user expects.
// One helper, one place to evolve the rule.
func RegisterAssetName(byAsset map[string]string, name, srcKey string) error {
	if name == cob.ProvenanceFile {
		return fmt.Errorf("uses the reserved asset name %q (cob writes that as the publish finalizer)", name)
	}
	if prev, dup := byAsset[name]; dup {
		return fmt.Errorf("collides with source %q — both publish as asset %q", prev, name)
	}
	byAsset[name] = srcKey
	return nil
}

// PromoteStageCount returns the number of promotion stages declared on a
// manifest, or 0 if the manifest has no promote block.
func PromoteStageCount(m *manifest.Manifest) int {
	if m.Promote == nil {
		return 0
	}
	return len(m.Promote.Stages)
}
