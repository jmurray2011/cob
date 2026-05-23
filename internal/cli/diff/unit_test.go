package diff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
)

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

// TestFileSHA256RefusesSymlink pins the dir-mode diff symlink defense:
// a symlink at the leaf must not redirect the integrity check to the
// file it points at. On unix this is enforced via O_NOFOLLOW (atomic);
// on Windows via Lstat-then-Open. Either way the error must call out
// that we're refusing to hash a symlink, not "ELOOP" kernel-speak.
func TestFileSHA256RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.bin")
	if err := os.WriteFile(target, []byte("real content"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.bin")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unsupported on this platform: %v", err)
	}
	_, err := fileSHA256(link)
	if err == nil {
		t.Fatal("fileSHA256 should refuse a symlink at the leaf; got nil error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should call out 'symlink' for the operator, got %v", err)
	}
	// The real file at the same dir is still hashable — the defense
	// targets symlinks specifically, not "anything in the dir".
	if _, err := fileSHA256(target); err != nil {
		t.Errorf("the regular file must still hash fine: %v", err)
	}
}
