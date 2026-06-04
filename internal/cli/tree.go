package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"

	"github.com/jmurray2011/cob/internal/cliutil"
)

func newTreeCmd(cfg *cliutil.Config) *cobra.Command {
	var flagDepth string

	cmd := &cobra.Command{
		Use:   "tree [coordinates]",
		Short: "Show the namespace as a tree (multi-level discovery)",
		Long: "Walks the CodeArtifact hierarchy from the given starting point " +
			"(or every domain if none) and renders it as an indented tree. " +
			"The default stops at packages — version count and latest version " +
			"are shown inline at no extra API cost (ListPackages already " +
			"returns them). Use --depth versions or --depth assets to descend " +
			"further; each level adds one list call per parent. Listing " +
			"errors on individual branches don't abort the walk — they're " +
			"shown inline, with a summary count at the end.\n\n" +
			"If you target a node and omit --depth, the default descends one " +
			"level into it: `cob tree acme/dev/tools/my-app` shows the " +
			"package's versions, `cob tree acme/dev` shows the repo's " +
			"packages.",
		Example: `  # tree of everything down to packages (default)
  cob tree

  # tree of one domain
  cob tree acme

  # tree of one repo, descend to versions
  cob tree acme/dev --depth versions

  # tree of one package's versions and assets
  cob tree acme/dev/tools/my-app --depth assets`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			return runTree(cmd.Context(), cfg, cmd, target, flagDepth)
		},
	}
	cmd.Flags().StringVar(&flagDepth, "depth", "packages", "Walk depth: domains|repos|packages|versions|assets")
	return cmd
}

// runTree resolves the start coordinates, picks a final depth (honoring an
// explicit --depth, otherwise descending one level under the targeted
// node), walks the hierarchy, and renders the result.
func runTree(ctx context.Context, cfg *cliutil.Config, cmd *cobra.Command, target, depthFlag string) error {
	out := cliutil.NewWriter(cfg)
	defer out.Close()

	depth, err := parseTreeDepth(depthFlag)
	if err != nil {
		return cliutil.Fail(out, "tree", cob.ExitError, "%s", err)
	}

	var start *cob.PackageCoordinates
	if target != "" {
		start, err = manifest.ParseCoordinates(target)
		if err != nil {
			return cliutil.Fail(out, "tree", cob.ExitError, "%s", err)
		}
	}

	// When the user targets a node and doesn't override --depth, descend
	// one level into it — `cob tree acme/dev/tools/my-app` showing only
	// "tools/my-app" with no children would be a useless default.
	if start != nil && !cmd.Flags().Changed("depth") {
		startKind := startDepthOf(start)
		if depth <= startKind && startKind < DepthAssets {
			depth = startKind + 1
		}
	}
	// Explicit --depth shallower than the target is a usage error — the
	// walk would produce nothing meaningful.
	if start != nil && cmd.Flags().Changed("depth") {
		startKind := startDepthOf(start)
		if depth < startKind {
			return cliutil.Fail(out, "tree", cob.ExitError,
				"--depth %s is shallower than the target (level %s); pick a deeper depth or drop the target",
				depthName(depth), depthName(startKind))
		}
	}

	client, err := cliutil.DialClient(ctx, cfg, out)
	if err != nil {
		return cliutil.Fail(out, "tree", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)

	root := walkHierarchy(ctx, registry, start, depth)
	// JSON shape is a flat array of typed records (one per node, walk
	// order), not the internal tree — see FlatNode. jq consumers filter
	// with `.[] | select(.kind == "package")` rather than recursing.
	if out.JSON(flattenForJSON(root)) {
		return nil
	}
	renderTreeText(out, root)
	if errs := countErrors(root); errs > 0 {
		out.Warn("%d branch(es) could not be listed (errors shown inline)", errs)
	}
	return nil
}

// renderTreeText writes the tree to stdout. The root node ("root" kind,
// representing the whole-world walk) has no name — its children are
// printed at column 0 to avoid a meaningless top line. A targeted walk
// returns a real node, which is printed as the tree's head.
func renderTreeText(out *output.Writer, n *TreeNode) {
	if n.Kind == "root" {
		if n.Error != "" {
			out.Plain("(failed) %s", n.Error)
			return
		}
		if len(n.Children) == 0 {
			out.Plain("(no domains)")
			return
		}
		for i, c := range n.Children {
			renderSubtree(out, c, "", i == len(n.Children)-1)
		}
		return
	}
	out.Plain("%s", treeLabel(n))
	for i, c := range n.Children {
		renderSubtree(out, c, "", i == len(n.Children)-1)
	}
}

// renderSubtree prints n and its descendants. prefix is the indentation
// already accumulated; isLast picks the box-drawing characters so the
// vertical bar drops only on non-last branches.
func renderSubtree(out *output.Writer, n *TreeNode, prefix string, isLast bool) {
	connector, childPrefix := "├── ", prefix+"│   "
	if isLast {
		connector, childPrefix = "└── ", prefix+"    "
	}
	out.Plain("%s%s%s", prefix, connector, treeLabel(n))
	for i, c := range n.Children {
		renderSubtree(out, c, childPrefix, i == len(n.Children)-1)
	}
}

// treeLabel formats a node's display string: name, optional meta in
// parentheses, optional inline error. The meta string is derived on the
// fly from the same typed fields the JSON output emits — there is no
// stored "meta" anywhere, so text and JSON can't drift apart.
func treeLabel(n *TreeNode) string {
	if n.Error != "" {
		return fmt.Sprintf("%s  ! %s", n.Name, n.Error)
	}
	meta := labelMeta(n)
	if meta == "" {
		return n.Name
	}
	return fmt.Sprintf("%s  (%s)", n.Name, meta)
}
