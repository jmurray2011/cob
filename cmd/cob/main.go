package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/jmurray2011/cob/internal/cli"
	"github.com/jmurray2011/cob/pkg/cob"
)

var version = "dev"

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	root := cli.NewRootCmd(resolveVersion())
	if err := root.Execute(); err != nil {
		// ExitError already had its message emitted by the command layer;
		// just carry the code out. Anything else is unexpected — print it.
		var ee *cli.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(cob.ExitError)
	}
}
