package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/jmurray2011/cob/internal/cli"
	"github.com/jmurray2011/cob/internal/cob"
)

var version = "dev"

// resolveBuildInfo returns the structured build metadata cob carries: the
// version string (from -ldflags or VCS), the VCS revision/time the
// toolchain embeds, and the Go runtime. NewRootCmd renders this both as
// cobra's --version line and as `cob version --json` for CI consumers.
func resolveBuildInfo() cli.BuildInfo {
	info, ok := debug.ReadBuildInfo()
	v := version
	if v == "dev" && ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	bi := cli.BuildInfo{Version: v}
	if !ok {
		return bi
	}
	bi.Go = info.GoVersion
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				bi.Commit = s.Value[:12]
			} else {
				bi.Commit = s.Value
			}
		case "vcs.time":
			bi.Time = s.Value
		}
	}
	return bi
}

func main() {
	// Ctrl-C / SIGTERM cancels the command's context so in-flight AWS calls
	// abort and deferred cleanup (temp files) runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// After the first signal, restore default handling so a second Ctrl-C
	// force-quits — otherwise a hung cleanup would be uninterruptible.
	go func() {
		<-ctx.Done()
		stop()
	}()

	root := cli.NewRootCmd(resolveBuildInfo())
	if err := root.ExecuteContext(ctx); err != nil {
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
