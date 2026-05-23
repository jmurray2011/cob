package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func newResolveCmd(cfg *cliutil.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve <coordinates>",
		Short: "Resolve the latest version of a package",
		Long:  "Resolves the most recently published version by timestamp and prints the version string. Designed for scripting: VERSION=$(cob resolve domain/repo/ns/pkg).",
		Example: `  # print the latest published version (for scripting)
  VERSION=$(cob resolve acme/dev/tools/my-app)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResolve(cmd.Context(), cfg, args)
		},
	}
	return cmd
}

func runResolve(ctx context.Context, cfg *cliutil.Config, args []string) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()

	target, err := cliutil.ResolveTarget(cfg, out, args, "resolve")
	if err != nil {
		return cliutil.Fail(out, "resolve", cob.ExitError, "%s", err)
	}

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return cliutil.Fail(out, "resolve", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return cliutil.Fail(out, "resolve", cob.ExitError, "full coordinates required (domain/repo/namespace/package)")
	}

	client, err := cliutil.DialClient(ctx, cfg)
	if err != nil {
		return cliutil.Fail(out, "resolve", cob.ExitError, "%s", err)
	}

	registry := cob.NewRegistry(client)

	// Strip @latest if provided -- resolve always means latest.
	if coords.Version != "" && coords.Version != "latest" {
		return cliutil.Fail(out, "resolve", cob.ExitError, "resolve always returns the latest version; got @%s", coords.Version)
	}

	version, err := registry.ResolveLatest(ctx, coords)
	if err != nil {
		return cliutil.Fail(out, "resolve", cliutil.CodeFor(err), "%s", err)
	}

	if cfg.JSON {
		result := struct {
			Package    string `json:"package"`
			Repository string `json:"repository"`
			Version    string `json:"version"`
		}{
			Package:    fmt.Sprintf("%s/%s", coords.Namespace, coords.Package),
			Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
			Version:    version,
		}
		enc := json.NewEncoder(out.Stdout())
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Fprintln(out.Stdout(), version)
	return nil
}
