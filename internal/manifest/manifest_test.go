package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestSHA256(t *testing.T) {
	if (&Manifest{}).SHA256() != "" {
		t.Error("a manifest without Raw must report an empty SHA256")
	}
	dir := t.TempDir()
	body := "domain: d\nrepository: r\nnamespace: n\npackage: p\nsources:\n  a: ./x\n"
	path := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Raw) != body {
		t.Errorf("Raw = %q, want the verbatim file bytes", m.Raw)
	}
	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); m.SHA256() != want {
		t.Errorf("SHA256 = %q, want %q", m.SHA256(), want)
	}
}

func TestExpandVars(t *testing.T) {
	t.Setenv("COB_VAR_GIT_SHA", "abc123") // ${env.GIT_SHA} reads COB_VAR_GIT_SHA

	tests := []struct {
		name    string
		in      string
		version string
		want    string
		wantErr bool
	}{
		{name: "version", in: "s3://b/app-${VERSION}.jar", version: "1.0.0", want: "s3://b/app-1.0.0.jar"},
		{name: "env", in: "s3://b/${env.GIT_SHA}/x", version: "1.0.0", want: "s3://b/abc123/x"},
		{name: "both", in: "${env.GIT_SHA}-${VERSION}", version: "9", want: "abc123-9"},
		{name: "no version provided", in: "a-${VERSION}", version: "", wantErr: true},
		{name: "unset env", in: "${env.NOPE_NOT_SET}", version: "1", wantErr: true},
		{name: "empty env name", in: "${env.}", version: "1", wantErr: true},
		{name: "unknown var", in: "${WHATEVER}", version: "1", wantErr: true},
		{name: "no vars", in: "s3://b/plain", version: "1", want: "s3://b/plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandVars(tt.in, tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveVariablesErrorNamesAsset(t *testing.T) {
	m := &Manifest{Sources: []SourceEntry{{Name: "bad", URI: "x-${env.DEFINITELY_UNSET_XYZ}"}}}
	err := m.ResolveVariables("1.0.0")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !contains(got, `asset "bad"`) {
		t.Fatalf("error should name the asset, got: %v", got)
	}
}

func TestInferPromoteSource(t *testing.T) {
	m := &Manifest{Promote: &PromoteConfig{Stages: []string{"dev", "staging", "prod"}}}

	if _, err := (&Manifest{}).InferPromoteSource("staging"); err == nil {
		t.Error("expected error when promote is nil")
	}
	if src, err := m.InferPromoteSource("staging"); err != nil || src != "dev" {
		t.Errorf("staging: got (%q,%v), want (dev,nil)", src, err)
	}
	if src, err := m.InferPromoteSource("prod"); err != nil || src != "staging" {
		t.Errorf("prod: got (%q,%v), want (staging,nil)", src, err)
	}
	if _, err := m.InferPromoteSource("dev"); err == nil {
		t.Error("expected error promoting to first stage")
	}
	if _, err := m.InferPromoteSource("nope"); err == nil {
		t.Error("expected error for unknown stage")
	}
}

func TestValidate(t *testing.T) {
	base := func() *Manifest {
		return &Manifest{
			Domain: "d", Repository: "r", Namespace: "n", Package: "p",
			Sources: []SourceEntry{{Name: "a", URI: "s3://b/k"}},
		}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	m := base()
	m.Domain = ""
	if err := m.validate(); err == nil {
		t.Error("expected error for missing domain")
	}

	m = base()
	m.Sources = nil
	if err := m.validate(); err == nil {
		t.Error("expected error for no sources")
	}

	m = base()
	m.Sources = []SourceEntry{{Name: "a", URI: ""}}
	if err := m.validate(); err == nil {
		t.Error("expected error for empty URI")
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("COB_DOMAIN", "override-dom")
	m := &Manifest{Domain: "orig", Repository: "r", Namespace: "n", Package: "p"}
	overrides := m.applyEnvOverrides()
	if m.Domain != "override-dom" {
		t.Fatalf("domain not overridden: %q", m.Domain)
	}
	if len(overrides) != 1 || overrides[0].Field != "domain" || overrides[0].Env != "COB_DOMAIN" || overrides[0].Value != "override-dom" {
		t.Fatalf("unexpected overrides: %+v", overrides)
	}
}

func TestLoadPreservesSourceOrderAndDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	yaml := `domain: d
repository: r
namespace: n
package: p
sources:
  zeta: s3://b/z
  alpha: ./a.txt
  middle: ca://d/r/n/p@1.0.0/x
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantOrder := []string{"zeta", "alpha", "middle"}
	if len(m.Sources) != 3 {
		t.Fatalf("got %d sources, want 3", len(m.Sources))
	}
	for i, w := range wantOrder {
		if m.Sources[i].Name != w {
			t.Errorf("source[%d] = %q, want %q (order must be preserved)", i, m.Sources[i].Name, w)
		}
	}
	if m.Dir != dir {
		t.Errorf("Dir = %q, want %q", m.Dir, dir)
	}
}

func TestLoadInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(path, []byte("domain: d\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for manifest with no sources")
	}
}

func TestLoadStrictDecode(t *testing.T) {
	valid := `domain: d
repository: r
namespace: n
package: p
sources:
  a: s3://b/k
`
	tests := []struct {
		name string
		yaml string
	}{
		{"unknown top-level key", "repositroy: r\n" + valid},
		{"unknown key under promote", valid + "promote:\n  stagess: [dev]\n"},
		{"empty file", ""},
		{"source value is not a scalar", `domain: d
repository: r
namespace: n
package: p
sources:
  a:
    nested: oops
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "m.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected Load to reject %s", tt.name)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
