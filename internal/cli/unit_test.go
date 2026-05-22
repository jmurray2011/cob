package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
)

func b(v bool) *bool { return &v }

func TestClassifyURI(t *testing.T) {
	cases := []struct {
		uri     string
		kind    uriKind
		wantErr bool
	}{
		{"s3://bucket/key", uriS3, false},
		{"ca://dom/repo/ns/pkg@1.0.0/asset.jar", uriCA, false},
		{"./local.txt", uriFile, false},
		{"/abs/local.txt", uriFile, false},
		{"bare.txt", uriFile, false},
		// An unrecognised scheme is rejected, never silently treated as a
		// file path — publish and validate must agree on this.
		{"gs://bucket/key", 0, true},
		{"https://example.com/x", 0, true},
	}
	for _, c := range cases {
		kind, _, err := classifyURI(c.uri, "/manifest/dir")
		if c.wantErr {
			if err == nil {
				t.Errorf("classifyURI(%q) should reject an unknown scheme", c.uri)
			}
			continue
		}
		if err != nil || kind != c.kind {
			t.Errorf("classifyURI(%q) = (%d, %v), want kind %d", c.uri, kind, err, c.kind)
		}
	}
}

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

func TestSafeJoin(t *testing.T) {
	root := "/out"
	ok := []struct{ name, wantSuffix string }{
		{"app.bin", "/out/app.bin"},
		{"sub/dir/app.bin", "/out/sub/dir/app.bin"},
		{"/etc/passwd", "/out/etc/passwd"}, // absolute-looking name stays under root
	}
	for _, c := range ok {
		got, err := safeJoin(root, c.name)
		if err != nil || got != c.wantSuffix {
			t.Errorf("safeJoin(%q,%q) = (%q,%v), want (%q,nil)", root, c.name, got, err, c.wantSuffix)
		}
	}
	for _, bad := range []string{"../../etc/passwd", "..", "sub/../../escape", "../sibling"} {
		if _, err := safeJoin(root, bad); err == nil {
			t.Errorf("safeJoin(%q,%q) should reject traversal", root, bad)
		}
	}
}

func TestSafeJoinRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// A symlinked subdirectory of root that points outside root: a lexical
	// check passes "evil/x.bin" but the write would land in `outside`.
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := safeJoin(root, "evil/x.bin"); err == nil {
		t.Error("safeJoin must reject a path that escapes via a symlinked subdirectory")
	}

	// A real (non-symlinked) subdirectory is still fine.
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(root, "real/x.bin"); err != nil {
		t.Errorf("safeJoin rejected a legitimate subdirectory: %v", err)
	}
}
