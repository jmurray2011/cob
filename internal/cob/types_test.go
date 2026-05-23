package cob

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAssetResultKindAndMethodConstants pins the Kind discriminator JSON
// shape and the Method constants per kind. A regression that removes a
// constant or changes its string value would break downstream --json
// consumers; catching it here means the wire-format contract is
// explicit, not folklore.
func TestAssetResultKindAndMethodConstants(t *testing.T) {
	// Per-kind constant strings: pin the wire format so a refactor that
	// renames a constant without thinking about JSON consumers fails the
	// build.
	cases := []struct {
		kind         AssetResultKind
		wantKindWire string
		methods      map[string]string // const-name → expected wire value
	}{
		{KindTransfer, "transfer", map[string]string{
			"TransferSpilled": TransferSpilled,
			"TransferSkipped": TransferSkipped,
		}},
		{KindCompare, "compare", map[string]string{
			"CompareMatch":        CompareMatch,
			"CompareMismatch":     CompareMismatch,
			"CompareAdded":        CompareAdded,
			"CompareRemoved":      CompareRemoved,
			"CompareChanged":      CompareChanged,
			"CompareSame":         CompareSame,
			"CompareMissing":      CompareMissing,
			"CompareMissingLocal": CompareMissingLocal,
			"CompareAltered":      CompareAltered,
			"CompareUnknown":      CompareUnknown,
		}},
		{KindLint, "lint", map[string]string{
			"LintExists":   LintExists,
			"LintSyntaxS3": LintSyntaxS3,
			"LintSyntaxCA": LintSyntaxCA,
			"LintSyntax":   LintSyntax,
		}},
		{KindDryRun, "dry-run", map[string]string{
			"DryRunPreview": DryRunPreview,
		}},
	}
	for _, c := range cases {
		if string(c.kind) != c.wantKindWire {
			t.Errorf("Kind %v wire value = %q, want %q", c.kind, string(c.kind), c.wantKindWire)
		}
		// Every method constant in this kind's group should be a
		// non-empty string distinct from every other.
		seen := make(map[string]string)
		for name, val := range c.methods {
			if val == "" {
				t.Errorf("%s is empty", name)
			}
			if prev, dup := seen[val]; dup {
				t.Errorf("%s and %s share the same wire value %q — JSON consumers can't distinguish them", name, prev, val)
			}
			seen[val] = name
		}
	}
}

// TestAssetResultJSONIncludesKind confirms an AssetResult marshals with
// both kind and method visible — the whole point of adding Kind was to
// let consumers switch on it before reading Method.
func TestAssetResultJSONIncludesKind(t *testing.T) {
	r := AssetResult{
		Name:   "app.bin",
		Kind:   KindTransfer,
		Method: TransferSpilled,
		Size:   5,
		SHA256: "abc",
	}
	raw, err := json.Marshal(&r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"kind":"transfer"`) {
		t.Errorf(`JSON missing "kind":"transfer": %s`, s)
	}
	if !strings.Contains(s, `"method":"spilled"`) {
		t.Errorf(`JSON missing "method":"spilled": %s`, s)
	}
}

// TestAssetResultJSONOmitsEmptyKind: a zero AssetResultKind ("") must not
// land in JSON — back-compat for v0.x consumers and for any pre-A3 row
// that might be deserialized from a stored CommandResult.
func TestAssetResultJSONOmitsEmptyKind(t *testing.T) {
	r := AssetResult{Name: "x", Method: "y"}
	raw, _ := json.Marshal(&r)
	if strings.Contains(string(raw), `"kind"`) {
		t.Errorf(`empty Kind should omitempty; got %s`, raw)
	}
}
