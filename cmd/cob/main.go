package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/jmurray2011/cob/internal/cli"
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
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
