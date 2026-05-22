package manifest

import "testing"

// FuzzParseCoordinates checks the coordinate parser never panics and never
// returns nil coordinates alongside a nil error, on any input string.
func FuzzParseCoordinates(f *testing.F) {
	for _, s := range []string{
		"", "dom", "dom/repo", "dom/repo/ns/pkg", "dom/repo/ns/pkg@1.0.0",
		"dom/*/ns/pkg@latest", "a/b/c", "///", "@", "x@", "dom/repo/ns/pkg@v@v",
		"  /  ", "dom//pkg", "dom/repo/ns/pkg@",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		coords, err := ParseCoordinates(s)
		if err == nil && coords == nil {
			t.Errorf("ParseCoordinates(%q): nil coordinates with a nil error", s)
		}
	})
}

// FuzzExpandVars checks variable expansion never panics, on any URI/version.
func FuzzExpandVars(f *testing.F) {
	seeds := []string{
		"plain", "${VERSION}", "${env.X}", "${env.}", "${", "}{", "${}",
		"a-${VERSION}-${env.GIT}", "$${VERSION}", "${VERSION", "${${}}",
	}
	for _, s := range seeds {
		f.Add(s, "1.0.0")
	}
	f.Fuzz(func(t *testing.T, s, version string) {
		_, _ = expandVars(s, version) // must not panic; result is irrelevant
	})
}
