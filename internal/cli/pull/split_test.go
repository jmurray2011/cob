package pull

import "testing"

// TestSplitSourceAndFilter pins the parser that divides "SOURCE:filter"
// into the coords/manifest part and the inline asset list. A manifest path
// is never split — it can carry a drive-letter ':' on Windows
// (C:\dir\m.yaml) — so it passes through whole. Otherwise the first ':'
// splits: coord segments are alphanumeric +.-_ and version is
// [a-zA-Z0-9.+-]+, so ':' is never legal before the filter in coordinates.
//
// The known limitation: ':' IS legal in CodeArtifact generic asset
// names. A user with `foo:bar.jar` as an asset can't filter to that
// name via this syntax (they'd have to pull the whole version, then
// pick the file out of the destination). That's an accepted
// trade-off for the cleaner shape.
func TestSplitSourceAndFilter(t *testing.T) {
	cases := []struct {
		in         string
		wantSource string
		wantFilter string
	}{
		// No filter — full positional is the source.
		{"acme/dev/tools/app@2.1.0", "acme/dev/tools/app@2.1.0", ""},
		{"./my-package.yaml", "./my-package.yaml", ""},
		{"", "", ""},

		// Windows manifest paths carry a drive-letter ':' that must NOT be
		// read as the filter separator — the whole path is the source.
		{`C:\build\cob-manifest.yaml`, `C:\build\cob-manifest.yaml`, ""},
		{`D:\pkgs\m.yml`, `D:\pkgs\m.yml`, ""},

		// Single inline filter.
		{"acme/dev/tools/app@2.1.0:foo.jar", "acme/dev/tools/app@2.1.0", "foo.jar"},

		// Multi-asset filter (comma-separated within the filter; the
		// splitter just hands the raw filter to selectAssets which
		// already handles commas).
		{"acme/dev/tools/app@2.1.0:foo.jar,bar.zip,baz.txt",
			"acme/dev/tools/app@2.1.0", "foo.jar,bar.zip,baz.txt"},

		// Shorthand forms — split before ResolveTarget sees them, so
		// the merge logic still receives clean coords.
		{"@latest:foo.jar", "@latest", "foo.jar"},
		{"ns/pkg@1.0.0:foo.jar", "ns/pkg@1.0.0", "foo.jar"},

		// Edge: trailing ':' is an empty filter — same as no filter
		// after splitting. selectAssets treats an empty filter string
		// as "all assets", so this Just Works without a special case.
		{"acme/dev/tools/app@2.1.0:", "acme/dev/tools/app@2.1.0", ""},

		// Edge: ':' as first char is also a "no source, just a filter"
		// shape. Unusual but the splitter doesn't reject it; downstream
		// ResolveTarget will fail because the source is empty AND there
		// is no @-prefix nor ns/pkg shape to merge.
		{":foo.jar", "", "foo.jar"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			source, filter := splitSourceAndFilter(c.in)
			if source != c.wantSource || filter != c.wantFilter {
				t.Errorf("splitSourceAndFilter(%q) = (%q, %q), want (%q, %q)",
					c.in, source, filter, c.wantSource, c.wantFilter)
			}
		})
	}
}
