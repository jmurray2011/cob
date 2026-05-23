package cli

import (
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func TestClassifyLs(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		want    lsKind
		invalid bool
	}{
		{name: "no target -> domains", target: "", want: lsKindDomains},
		{name: "domain -> repos", target: "acme", want: lsKindRepos},
		{name: "domain/repo -> packages", target: "acme/dev", want: lsKindPackages},
		{name: "wildcard repo, no ns -> invalid", target: "acme/*", invalid: true},
		{name: "full coords -> versions", target: "acme/dev/ns/pkg", want: lsKindVersions},
		{name: "wildcard full no version -> invalid", target: "acme/*/ns/pkg", invalid: true},
		{name: "wildcard full with version -> promotion", target: "acme/*/ns/pkg@1.0.0", want: lsKindPromotion},
		{name: "full coords with version -> assets", target: "acme/dev/ns/pkg@1.0.0", want: lsKindAssets},
		{name: "full coords @latest -> assets (resolved in dispatch)", target: "acme/dev/ns/pkg@latest", want: lsKindAssets},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coords := parseForTest(t, tt.target)
			got, invalid := classifyLs(coords, tt.target)
			if tt.invalid {
				if invalid == "" {
					t.Fatalf("expected invalid combination for %q, got kind %v", tt.target, got)
				}
				return
			}
			if invalid != "" {
				t.Fatalf("unexpected invalid message: %q", invalid)
			}
			if got != tt.want {
				t.Fatalf("classifyLs(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func parseForTest(t *testing.T, target string) *cob.PackageCoordinates {
	t.Helper()
	if target == "" {
		return nil
	}
	c, err := manifest.ParseCoordinates(target)
	if err != nil {
		t.Fatalf("ParseCoordinates(%q): %v", target, err)
	}
	return c
}
