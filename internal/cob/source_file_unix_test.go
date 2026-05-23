//go:build unix

package cob

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFileSourceResolveRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	// Without the IsRegular check, Open(fifo) blocks until a writer opens
	// the other end — Resolve must reject it instead.
	if _, err := NewFileSource(p, "./fifo").Resolve(context.Background()); err == nil {
		t.Error("Resolve must reject a FIFO")
	}
}
