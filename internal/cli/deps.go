package cli

import (
	"context"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// Test seams. Production code uses the real constructors; tests reassign
// these to inject an in-memory AWS client and to capture output into
// buffers. They are package-level vars rather than command parameters so the
// cobra wiring and run* signatures stay unchanged.
var (
	newClient = cob.NewClient
	newWriter = output.New
)

// dialClient builds the AWS client from the current global flags. Every
// command goes through it, so a flag affecting client construction is wired
// in exactly one place.
func dialClient(ctx context.Context) (*cob.Client, error) {
	return newClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
		Debug:   flagDebug,
		TmpDir:  flagTmpDir,
	})
}
