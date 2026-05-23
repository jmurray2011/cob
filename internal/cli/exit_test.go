package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

func TestFailJSONMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.NewWithWriters(&stdout, &stderr, output.Mode{JSON: true})

	err := fail(w, "publish", cob.ExitConflict, "version %s already exists", "1.0.0")

	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("fail should return *ExitError, got %T", err)
	}
	if ee.Code != cob.ExitConflict {
		t.Errorf("Code = %d, want %d", ee.Code, cob.ExitConflict)
	}

	// JSON CommandResult must land on stdout so CI can parse it.
	var got cob.CommandResult
	if e := json.Unmarshal(stdout.Bytes(), &got); e != nil {
		t.Fatalf("stdout is not valid JSON (%v): %q", e, stdout.String())
	}
	if got.Command != "publish" || got.Status != "error" || got.Error != "version 1.0.0 already exists" {
		t.Errorf("unexpected CommandResult: %+v", got)
	}
	if !strings.Contains(stderr.String(), "version 1.0.0 already exists") {
		t.Errorf("stderr should carry the human error, got %q", stderr.String())
	}
}

func TestFailHumanMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.NewWithWriters(&stdout, &stderr, output.Mode{})

	_ = fail(w, "pull", cob.ExitNotFound, "nope")

	if stdout.Len() != 0 {
		t.Errorf("human mode must not write JSON to stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Error: nope") {
		t.Errorf("stderr = %q, want it to contain 'Error: nope'", stderr.String())
	}
}

func TestExitErrorMessage(t *testing.T) {
	e := &ExitError{Code: 3}
	if e.Error() == "" {
		t.Error("ExitError.Error() should be non-empty")
	}
}
