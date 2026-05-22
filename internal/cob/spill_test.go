package cob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpillToTemp(t *testing.T) {
	const content = "hello cob, this spills to a temp file"
	want := sha256.Sum256([]byte(content))
	wantHex := hex.EncodeToString(want[:])

	dir := t.TempDir()
	ta, err := spillToTemp(strings.NewReader(content), dir)
	if err != nil {
		t.Fatalf("spillToTemp: %v", err)
	}
	name := ta.f.Name()

	// The spill file must land in the directory we asked for.
	if filepath.Dir(name) != dir {
		t.Errorf("spill file %s not under requested dir %s", name, dir)
	}

	if ta.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", ta.Size, len(content))
	}
	if ta.SHA256 != wantHex {
		t.Errorf("SHA256 = %s, want %s", ta.SHA256, wantHex)
	}

	// Must be rewound and readable as the upload ReadSeeker.
	got, err := io.ReadAll(ta.f)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}

	if err := ta.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("temp file %s not removed after Close (stat err=%v)", name, err)
	}
	if err := ta.Close(); err != nil {
		t.Errorf("second Close should be a safe no-op, got %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestSpillToTempReadError(t *testing.T) {
	if _, err := spillToTemp(errReader{}, ""); err == nil {
		t.Fatal("expected an error when the source read fails")
	}
}
