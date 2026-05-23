package cli

import (
	"context"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// Test seams. Production code uses the real constructors; tests reassign
// these to inject an in-memory AWS client and to capture output into
// buffers. They are function indirections, not state — Config holds the
// runtime configuration; these two pick which implementation of "build an
// AWS client" or "build an output writer" we get.
var (
	newClient = cob.NewClient
	// newWriter builds the output writer from cfg. The mode (JSON / quiet
	// / no-TUI) is forwarded to the output package, which picks a
	// renderer accordingly. Each runXxx defers out.Close() so the live
	// renderer's bubbletea program finishes painting before the shell
	// prompt returns.
	newWriter = func(cfg *Config) *output.Writer {
		return output.New(output.Mode{
			JSON:  cfg.JSON,
			Quiet: cfg.Quiet,
			NoTUI: cfg.NoTUI,
		})
	}
)

// dialClient builds the AWS client from cfg. Every command goes through it,
// so a flag affecting client construction is wired in one place.
func dialClient(ctx context.Context, cfg *Config) (*cob.Client, error) {
	return newClient(ctx, cob.ClientOptions{
		Profile: cfg.Profile,
		Region:  cfg.Region,
		Debug:   cfg.Debug,
		TmpDir:  cfg.TmpDir,
	})
}
