package main

import (
	"fmt"
	"os"

	"github.com/jmurray2011/cob/internal/cli"
)

var version = "dev"

func main() {
	root := cli.NewRootCmd(version)
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
