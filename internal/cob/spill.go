package cob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// tempAsset is a source's bytes streamed to a temporary file. It satisfies
// the io.ReadSeeker that PublishPackageVersion needs (for Content-Length and
// retries) without holding the whole asset in memory. Close removes the
// backing file; callers must always Close it.
type tempAsset struct {
	f      *os.File
	Size   int64
	SHA256 string
}

func (t *tempAsset) Close() error {
	if t.f == nil {
		return nil
	}
	name := t.f.Name()
	err := t.f.Close()
	_ = os.Remove(name)
	t.f = nil
	return err
}

// countingReader reports each chunk's byte count to report (when non-nil)
// as the wrapped reader is consumed — it drives per-asset transfer-progress
// display. The asset name is bound at construction so a single Progress
// callback can drive a multi-asset live view without races.
type countingReader struct {
	r      io.Reader
	name   string
	report func(name string, delta int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.report != nil {
		c.report(c.name, int64(n))
	}
	return n, err
}

// spillToTemp streams r into a temp file under dir (or the OS default temp
// directory when dir is ""), computing SHA-256 in the same pass, and rewinds
// it ready for upload. Memory stays O(buffer) regardless of asset size, so
// there is no size ceiling. report, when non-nil, receives (name, delta)
// byte updates as the stream is consumed; name lets one Progress callback
// drive per-asset rows in a live display.
func spillToTemp(r io.Reader, dir, name string, report func(name string, delta int64)) (*tempAsset, error) {
	f, err := os.CreateTemp(dir, "cob-asset-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	clean := func(e error) (*tempAsset, error) {
		f.Close()
		os.Remove(f.Name())
		return nil, e
	}

	if report != nil {
		r = &countingReader{r: r, name: name, report: report}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return clean(fmt.Errorf("buffering to temp file: %w", err))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return clean(fmt.Errorf("rewinding temp file: %w", err))
	}
	return &tempAsset{f: f, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
