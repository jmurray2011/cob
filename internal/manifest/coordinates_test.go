package manifest

import "testing"

func TestParseCoordinates(t *testing.T) {
	tests := []struct {
		name                    string
		in                      string
		wantErr                 bool
		dom, repo, ns, pkg, ver string
	}{
		{name: "domain only", in: "acme", dom: "acme"},
		{name: "domain/repo", in: "acme/dev", dom: "acme", repo: "dev"},
		{name: "full", in: "acme/dev/ns/pkg", dom: "acme", repo: "dev", ns: "ns", pkg: "pkg"},
		{name: "full with version", in: "acme/dev/ns/pkg@1.2.3", dom: "acme", repo: "dev", ns: "ns", pkg: "pkg", ver: "1.2.3"},
		{name: "latest", in: "acme/dev/ns/pkg@latest", dom: "acme", repo: "dev", ns: "ns", pkg: "pkg", ver: "latest"},
		{name: "wildcard repo", in: "acme/*/ns/pkg@1.0.0", dom: "acme", repo: "*", ns: "ns", pkg: "pkg", ver: "1.0.0"},
		{name: "trailing slash trimmed", in: "acme/dev/", dom: "acme", repo: "dev"},
		{name: "domain/repo with version", in: "acme/dev@9", dom: "acme", repo: "dev", ver: "9"},
		{name: "3 segments rejected", in: "acme/dev/pkg", wantErr: true},
		{name: "empty version after @", in: "acme/dev/ns/pkg@", wantErr: true},
		{name: "too many segments", in: "a/b/c/d/e", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseCoordinates(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Domain != tt.dom || c.Repository != tt.repo || c.Namespace != tt.ns || c.Package != tt.pkg || c.Version != tt.ver {
				t.Fatalf("got %+v, want dom=%q repo=%q ns=%q pkg=%q ver=%q",
					c, tt.dom, tt.repo, tt.ns, tt.pkg, tt.ver)
			}
		})
	}
}

func TestFormatCoordinatesRoundTrip(t *testing.T) {
	for _, in := range []string{
		"acme/dev",
		"acme/dev/ns/pkg",
		"acme/dev/ns/pkg@1.2.3",
	} {
		c, err := ParseCoordinates(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := FormatCoordinates(c); got != in {
			t.Errorf("round trip: got %q, want %q", got, in)
		}
	}
}
