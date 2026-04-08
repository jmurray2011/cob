package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
	"github.com/jmurray2011/cob/pkg/cob"
)

func newResolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve <coordinates>",
		Short: "Resolve the latest version of a package",
		Long:  "Resolves the most recently published version by timestamp and prints the version string. Designed for scripting: VERSION=$(cob resolve domain/repo/ns/pkg).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResolve(cmd.Context(), args[0])
		},
	}
	return cmd
}

func runResolve(ctx context.Context, target string) error {
	out := output.New(flagJSON)

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		out.ErrorResult("resolve", err.Error())
		os.Exit(cob.ExitError)
	}
	if coords.Namespace == "" || coords.Package == "" {
		out.ErrorResult("resolve", "full coordinates required (domain/repo/namespace/package)")
		os.Exit(cob.ExitError)
	}

	client, err := cob.NewClient(ctx, cob.ClientOptions{
		Profile: flagProfile,
		Region:  flagRegion,
	})
	if err != nil {
		out.ErrorResult("resolve", err.Error())
		os.Exit(cob.ExitError)
	}

	registry := cob.NewRegistry(client)

	// Strip @latest if provided -- resolve always means latest.
	if coords.Version != "" && coords.Version != "latest" {
		out.ErrorResult("resolve", fmt.Sprintf("resolve always returns the latest version; got @%s", coords.Version))
		os.Exit(cob.ExitError)
	}

	version, err := registry.ResolveLatest(ctx, coords)
	if err != nil {
		out.ErrorResult("resolve", err.Error())
		os.Exit(cob.ExitNotFound)
	}

	if flagJSON {
		result := struct {
			Package    string `json:"package"`
			Repository string `json:"repository"`
			Version    string `json:"version"`
		}{
			Package:    fmt.Sprintf("%s/%s", coords.Namespace, coords.Package),
			Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
			Version:    version,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Println(version)
	return nil
}
