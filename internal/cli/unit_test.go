package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
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

// TestInterruptableAppliesTimeout pins the --timeout / COB_TIMEOUT
// contract: when cfg.Timeout is set, the returned ctx is a deadline
// child of the input. A hung operation eventually surfaces as
// context.DeadlineExceeded so a CI pipeline doesn't sit on a stalled
// AWS call forever.
func TestInterruptableAppliesTimeout(t *testing.T) {
	cfg := &Config{Timeout: 5 * time.Millisecond}
	w := output.NewWithWriters(&dummyBuf{}, &dummyBuf{}, output.Mode{})
	ctx, cancel := interruptable(context.Background(), cfg, w)
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("timeout context did not fire within 1s; --timeout=5ms is not wired through")
	}
}

// TestInterruptableNoTimeoutByDefault: cfg.Timeout == 0 leaves the ctx
// without a deadline (operators on local dev runs should never be
// surprised by a clock they didn't set).
func TestInterruptableNoTimeoutByDefault(t *testing.T) {
	cfg := &Config{} // Timeout: 0 — no deadline
	w := output.NewWithWriters(&dummyBuf{}, &dummyBuf{}, output.Mode{})
	ctx, cancel := interruptable(context.Background(), cfg, w)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("ctx should have no deadline when cfg.Timeout == 0")
	}
}

// dummyBuf is a minimal io.Writer that discards everything — output.Writer
// only needs somewhere to send bytes, and the tests above only care about
// the ctx side of interruptable, not the renderer.
type dummyBuf struct{}

func (dummyBuf) Write(p []byte) (int, error) { return len(p), nil }

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
