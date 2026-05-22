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

// resolveBuildVersion returns a version string, enriched (when cob was built
// without an explicit -ldflags version) with the VCS revision/time and Go
// version that the toolchain embeds.
func resolveBuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	v := version
	if v == "dev" && ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	if !ok {
		return v
	}
	var rev, t string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.time":
			t = s.Value
		}
	}
	extra := info.GoVersion
	if rev != "" {
		extra = rev + ", " + extra
	}
	if t != "" {
		extra += ", " + t
	}
	if extra != "" {
		return v + " (" + extra + ")"
	}
	return v
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

	root := cli.NewRootCmd(resolveBuildVersion())
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
