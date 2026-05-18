package cli

import (
	"testing"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/pkg/cob"
)

func TestClassifyLs(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		allRepos bool
		want     lsKind
		invalid  bool
	}{
		{name: "no target -> domains", target: "", want: lsKindDomains},
		{name: "domain -> repos", target: "acme", want: lsKindRepos},
		{
			// Precedence guard: domain-only is classified before the
			// wildcard rules, so --all-repos on a bare domain still lists
			// repos rather than erroring.
			name: "domain --all-repos still repos", target: "acme", allRepos: true, want: lsKindRepos,
		},
		{name: "domain/repo -> packages", target: "acme/dev", want: lsKindPackages},
		{name: "domain/repo --all-repos -> invalid", target: "acme/dev", allRepos: true, invalid: true},
		{name: "wildcard repo, no ns -> invalid", target: "acme/*", invalid: true},
		{name: "full coords -> versions", target: "acme/dev/ns/pkg", want: lsKindVersions},
		{name: "full coords --all-repos no version -> invalid", target: "acme/dev/ns/pkg", allRepos: true, invalid: true},
		{name: "wildcard full no version -> invalid", target: "acme/*/ns/pkg", invalid: true},
		{name: "wildcard full with version -> promotion", target: "acme/*/ns/pkg@1.0.0", want: lsKindPromotion},
		{name: "full coords --all-repos with version -> promotion", target: "acme/dev/ns/pkg@1.0.0", allRepos: true, want: lsKindPromotion},
		{name: "full coords with version -> assets", target: "acme/dev/ns/pkg@1.0.0", want: lsKindAssets},
		{name: "full coords @latest -> assets (resolved in dispatch)", target: "acme/dev/ns/pkg@latest", want: lsKindAssets},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coords := parseForTest(t, tt.target)
			got, invalid := classifyLs(coords, tt.target, tt.allRepos)
			if tt.invalid {
				if invalid == "" {
					t.Fatalf("expected invalid combination for %q (allRepos=%v), got kind %v", tt.target, tt.allRepos, got)
				}
				return
			}
			if invalid != "" {
				t.Fatalf("unexpected invalid message: %q", invalid)
			}
			if got != tt.want {
				t.Fatalf("classifyLs(%q, allRepos=%v) = %v, want %v", tt.target, tt.allRepos, got, tt.want)
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
