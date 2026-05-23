package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
)

func newResolveCmd(cfg *Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve <coordinates>",
		Short: "Resolve the latest version of a package",
		Long:  "Resolves the most recently published version by timestamp and prints the version string. Designed for scripting: VERSION=$(cob resolve domain/repo/ns/pkg).",
		Example: `  # print the latest published version (for scripting)
  VERSION=$(cob resolve acme/dev/tools/my-app)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResolve(cmd.Context(), cfg, args[0])
		},
	}
	return cmd
}

func runResolve(ctx context.Context, cfg *Config, target string) error {
	out := newWriter(cfg)
	defer out.Close()

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "resolve", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "resolve", cob.ExitError, "full coordinates required (domain/repo/namespace/package)")
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "resolve", cob.ExitError, "%s", err)
	}

	registry := cob.NewRegistry(client)

	// Strip @latest if provided -- resolve always means latest.
	if coords.Version != "" && coords.Version != "latest" {
		return fail(out, "resolve", cob.ExitError, "resolve always returns the latest version; got @%s", coords.Version)
	}

	version, err := registry.ResolveLatest(ctx, coords)
	if err != nil {
		return fail(out, "resolve", codeFor(err), "%s", err)
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
