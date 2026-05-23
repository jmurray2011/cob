package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jmurray2011/cob/internal/cliutil"

	"github.com/jmurray2011/cob/internal/cliutil/clitest"
)

func TestBuildInfoHumanStringShape(t *testing.T) {
	cases := []struct {
		name string
		bi   cliutil.BuildInfo
		want string
	}{
		// Fully-populated build (a tagged release): every field present,
		// "v (commit, go, time)" shape — same as the cobra --version line.
		{"full", cliutil.BuildInfo{Version: "v1.2.3", Commit: "abc123", Time: "2026-05-01T00:00:00Z", Go: "go1.25.3"},
			"v1.2.3 (abc123, go1.25.3, 2026-05-01T00:00:00Z)"},
		// Local dev build with no embedded VCS info: commit/time blank,
		// just version + go.
		{"go only", cliutil.BuildInfo{Version: "dev", Go: "go1.25.3"}, "dev (go1.25.3)"},
		// Truly bare cliutil.BuildInfo (debug.ReadBuildInfo failed): version alone.
		{"version only", cliutil.BuildInfo{Version: "v1.2.3"}, "v1.2.3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.bi.HumanString(); got != c.want {
				t.Errorf("HumanString() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestVersionSubcommandJSONShape(t *testing.T) {
	// Contract: `cob version --json` emits a stable shape with these
	// exact keys; CI consumers pin on it. Runtime fields (go/os/arch)
	// are filled lazily so a pinned cliutil.BuildInfo doesn't need to spoof
	// them — assert they're present after the run rather than checking
	// exact values.
	cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{})
	cfg.JSON = true
	cfg.Build = cliutil.BuildInfo{Version: "v9.9.9", Commit: "deadbeef", Time: "2026-01-01T00:00:00Z"}

	cmd := newVersionCmd(cfg)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("version --json: %v", err)
	}
	var got cliutil.BuildInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("JSON parse: %v\nraw: %s", err, stdout.String())
	}
	if got.Version != "v9.9.9" || got.Commit != "deadbeef" || got.Time != "2026-01-01T00:00:00Z" {
		t.Errorf("structured fields lost in round-trip: %+v", got)
	}
	if got.Go == "" || got.OS == "" || got.Arch == "" {
		t.Errorf("runtime fields should be filled lazily, got %+v", got)
	}
}

func TestVersionSubcommandHumanModeOutput(t *testing.T) {
	// Default (non-JSON) emits the same parenthesized human shape as
	// the cobra --version line, so the two paths stay in lockstep.
	cfg, stdout, _ := clitest.UseFake(t, &clitest.FakeCA{})
	cfg.Build = cliutil.BuildInfo{Version: "v1.0.0", Commit: "abc123", Go: "go1.25.3"}
	cmd := newVersionCmd(cfg)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(stdout.String(), "v1.0.0 (abc123") {
		t.Errorf("human output should include parenthesized commit+go; got %q", stdout.String())
	}
}
