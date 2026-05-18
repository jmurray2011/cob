package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmurray2011/cob/pkg/cob"
)

func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	content := []byte("domain: d\n")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	want := func() string { s := sha256.Sum256(content); return hex.EncodeToString(s[:]) }()
	if got := fileSHA256(p); got != want {
		t.Errorf("fileSHA256 = %q, want %q", got, want)
	}
	if got := fileSHA256(filepath.Join(dir, "nope")); got != "" {
		t.Errorf("missing file should give \"\", got %q", got)
	}
}

func b(v bool) *bool { return &v }

func TestOriginS3Changed(t *testing.T) {
	cases := []struct {
		name     string
		rec, cur *cob.Origin
		want     bool
	}{
		{"current unreadable -> changed", &cob.Origin{ETag: "e"}, nil, true},
		{"versioned same version_id -> unchanged",
			&cob.Origin{Versioned: b(true), VersionID: "v1", ETag: "old"},
			&cob.Origin{VersionID: "v1", ETag: "new"}, false},
		{"versioned different version_id -> changed",
			&cob.Origin{Versioned: b(true), VersionID: "v1"},
			&cob.Origin{VersionID: "v2"}, true},
		{"unversioned same etag -> unchanged",
			&cob.Origin{Versioned: b(false), ETag: "abc"},
			&cob.Origin{ETag: "abc"}, false},
		{"unversioned different etag -> changed",
			&cob.Origin{ETag: "abc"}, &cob.Origin{ETag: "xyz"}, true},
		{"nothing comparable -> unchanged (no false positive)",
			&cob.Origin{}, &cob.Origin{}, false},
	}
	for _, c := range cases {
		if got := originS3Changed(c.rec, c.cur); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
