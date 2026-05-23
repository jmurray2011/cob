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
	// newWriter builds the output writer from cfg, applying --quiet (which
	// output.New itself doesn't know about).
	newWriter = func(cfg *Config) *output.Writer {
		w := output.New(cfg.JSON)
		w.SetQuiet(cfg.Quiet)
		return w
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
