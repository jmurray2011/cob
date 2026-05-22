package cli

import (
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

// Test seams. Production code uses the real constructors; tests reassign
// these to inject an in-memory AWS client and to capture output into
// buffers. They are package-level vars rather than command parameters so the
// cobra wiring and run* signatures stay unchanged.
var (
	newClient = cob.NewClient
	newWriter = output.New
)
