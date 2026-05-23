package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// treeWalkConcurrency bounds in-flight CodeArtifact list calls during a
// hierarchy walk. Matches promotionStatusConcurrency — both are batches of
// independent reads against the same service.
const treeWalkConcurrency = 8

// TreeDepth names the deepest level a walk descends to. Absolute, not
// relative: --depth packages means "leaves are packages" regardless of
// whether the walk started at root, a domain, or a repo.
type TreeDepth int

const (
	DepthDomains TreeDepth = iota
	DepthRepos
	DepthPackages
	DepthVersions
	DepthAssets
)

// parseTreeDepth maps a --depth flag value to the enum. An empty value is
// the default (packages); an unrecognized value is rejected with a list of
// the allowed names rather than a silent fallback.
func parseTreeDepth(s string) (TreeDepth, error) {
	switch s {
	case "", "packages":
		return DepthPackages, nil
	case "domains":
		return DepthDomains, nil
	case "repos":
		return DepthRepos, nil
	case "versions":
		return DepthVersions, nil
	case "assets":
		return DepthAssets, nil
	default:
		return 0, fmt.Errorf("--depth must be one of: domains, repos, packages, versions, assets; got %q", s)
	}
}

func depthName(d TreeDepth) string {
	switch d {
	case DepthDomains:
		return "domains"
	case DepthRepos:
		return "repos"
	case DepthPackages:
		return "packages"
	case DepthVersions:
		return "versions"
	case DepthAssets:
		return "assets"
	}
	return "?"
}

// startDepthOf returns the level of the start coordinate itself — the
// shallowest depth a walk from there can use without truncating. Empty
// coords (whole-world walk) report DepthDomains; a start with a version
// reports DepthVersions; etc.
func startDepthOf(c *cob.PackageCoordinates) TreeDepth {
	switch {
	case c == nil || c.Domain == "":
		return DepthDomains
	case c.Repository == "":
		return DepthDomains
	case c.Namespace == "":
		return DepthRepos
	case c.Version == "":
		return DepthPackages
	default:
		return DepthVersions
	}
}

// TreeNode is one node of the walked hierarchy. Kind names the level; Path
// is the fully-qualified coordinate string (used by ls -R as its leaf form
// and as a stable identifier for JSON consumers); Meta is a short
// human-readable summary rendered after the name in tree mode (version
// count, latest, asset size, …). Children is nil when the walk stopped at
// this node (by depth, or because the level has no listable children).
// Error captures a per-node listing failure so one bad branch doesn't kill
// an org-wide discovery — the rest of the tree is still useful.
type TreeNode struct {
	Name     string      `json:"name"`
	Kind     string      `json:"kind"`
	Path     string      `json:"path,omitempty"`
	Meta     string      `json:"meta,omitempty"`
	Children []*TreeNode `json:"children,omitempty"`
	Error    string      `json:"error,omitempty"`
}

// walkHierarchy walks the CodeArtifact namespace under start (nil = every
// domain) down to depth and returns the resulting tree. Per-branch listing
// errors are recorded on the node, not propagated — discovery use cases
// want a partial tree, not an aborted command.
func walkHierarchy(ctx context.Context, registry *cob.Registry, start *cob.PackageCoordinates, depth TreeDepth) *TreeNode {
	w := &walker{registry: registry, sem: make(chan struct{}, treeWalkConcurrency), depth: depth}

	switch {
	case start == nil || start.Domain == "":
		root := &TreeNode{Kind: "root"}
		w.expandRoot(ctx, root)
		return root
	case start.Repository == "":
		n := &TreeNode{Name: start.Domain, Kind: "domain", Path: start.Domain}
		if depth >= DepthRepos {
			w.expandDomain(ctx, n, start.Domain)
		}
		return n
	case start.Namespace == "":
		n := &TreeNode{Name: start.Repository, Kind: "repo",
			Path: start.Domain + "/" + start.Repository}
		if depth >= DepthPackages {
			w.expandRepo(ctx, n, start.Domain, start.Repository)
		}
		return n
	case start.Version == "":
		path := fmt.Sprintf("%s/%s/%s/%s", start.Domain, start.Repository, start.Namespace, start.Package)
		n := &TreeNode{Name: start.Namespace + "/" + start.Package, Kind: "package", Path: path}
		if depth >= DepthVersions {
			w.expandPackage(ctx, n, *start)
		}
		return n
	default:
		path := fmt.Sprintf("%s/%s/%s/%s@%s", start.Domain, start.Repository, start.Namespace, start.Package, start.Version)
		n := &TreeNode{Name: start.Version, Kind: "version", Path: path}
		if depth >= DepthAssets {
			w.expandVersion(ctx, n, *start)
		}
		return n
	}
}

type walker struct {
	registry *cob.Registry
	sem      chan struct{}
	depth    TreeDepth
}

// throttled runs the API call holding one semaphore slot, then releases it
// before the caller fans out into children. Bounding only the API call (not
// the parent goroutine while it waits for children) prevents nested
// fan-outs from deadlocking on the semaphore — a recursive walk would
// otherwise deadlock the moment depth-N goroutines all held slots and
// tried to start depth-(N+1).
func (w *walker) throttled(fn func()) {
	w.sem <- struct{}{}
	defer func() { <-w.sem }()
	fn()
}

// recurse expands each child concurrently. Each child does its own API
// call under w.throttled, so the semaphore caps concurrent API calls
// without capping goroutines (which would deadlock here).
func (w *walker) recurse(ctx context.Context, children []*TreeNode, expand func(context.Context, *TreeNode)) {
	var wg sync.WaitGroup
	for _, c := range children {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(c *TreeNode) {
			defer wg.Done()
			expand(ctx, c)
		}(c)
	}
	wg.Wait()
}

func (w *walker) expandRoot(ctx context.Context, n *TreeNode) {
	var domains []cob.DomainSummary
	var err error
	w.throttled(func() { domains, err = w.registry.ListDomains(ctx) })
	if err != nil {
		n.Error = err.Error()
		return
	}
	n.Meta = pluralize(len(domains), "domain")
	for _, d := range domains {
		n.Children = append(n.Children, &TreeNode{Name: d.Name, Kind: "domain", Path: d.Name})
	}
	if w.depth <= DepthDomains {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		w.expandDomain(ctx, child, child.Name)
	})
}

func (w *walker) expandDomain(ctx context.Context, n *TreeNode, domain string) {
	var repos []string
	var err error
	w.throttled(func() { repos, err = w.registry.ListRepositories(ctx, domain) })
	if err != nil {
		n.Error = err.Error()
		return
	}
	n.Meta = pluralize(len(repos), "repo")
	for _, r := range repos {
		n.Children = append(n.Children, &TreeNode{Name: r, Kind: "repo", Path: domain + "/" + r})
	}
	if w.depth <= DepthRepos {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		w.expandRepo(ctx, child, domain, child.Name)
	})
}

func (w *walker) expandRepo(ctx context.Context, n *TreeNode, domain, repo string) {
	var pkgs []cob.PackageSummary
	var err error
	w.throttled(func() { pkgs, err = w.registry.ListPackages(ctx, domain, repo) })
	if err != nil {
		n.Error = err.Error()
		return
	}
	n.Meta = pluralize(len(pkgs), "package")
	for _, p := range pkgs {
		// PackageSummary already carries LatestVersion + VersionCount from
		// ListPackages, so the package row gets its summary at no extra API
		// cost — this is what makes depth=packages cheap.
		var meta string
		switch {
		case p.VersionCount == 0:
			meta = "no versions"
		case p.LatestVersion == "":
			meta = pluralize(p.VersionCount, "version")
		default:
			meta = fmt.Sprintf("%s, latest %s", pluralize(p.VersionCount, "version"), p.LatestVersion)
		}
		n.Children = append(n.Children, &TreeNode{
			Name: p.Namespace + "/" + p.Package,
			Kind: "package",
			Path: fmt.Sprintf("%s/%s/%s/%s", domain, repo, p.Namespace, p.Package),
			Meta: meta,
		})
	}
	if w.depth <= DepthPackages {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		ns, pkg, _ := strings.Cut(child.Name, "/")
		w.expandPackage(ctx, child, cob.PackageCoordinates{
			Domain: domain, Repository: repo, Namespace: ns, Package: pkg,
		})
	})
}

func (w *walker) expandPackage(ctx context.Context, n *TreeNode, c cob.PackageCoordinates) {
	var versions []cob.VersionSummary
	var err error
	w.throttled(func() { versions, err = w.registry.ListVersions(ctx, &c) })
	if err != nil {
		n.Error = err.Error()
		return
	}
	n.Meta = pluralize(len(versions), "version")
	for _, v := range versions {
		meta := pluralize(v.Assets, "asset")
		if !v.Published.IsZero() {
			meta += ", " + v.Published.Format("2006-01-02")
		}
		n.Children = append(n.Children, &TreeNode{
			Name: v.Version,
			Kind: "version",
			Path: fmt.Sprintf("%s/%s/%s/%s@%s", c.Domain, c.Repository, c.Namespace, c.Package, v.Version),
			Meta: meta,
		})
	}
	if w.depth <= DepthVersions {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		vc := c
		vc.Version = child.Name
		w.expandVersion(ctx, child, vc)
	})
}

func (w *walker) expandVersion(ctx context.Context, n *TreeNode, c cob.PackageCoordinates) {
	var assets []cob.AssetSummary
	var err error
	w.throttled(func() { assets, err = w.registry.ListAssets(ctx, &c) })
	if err != nil {
		n.Error = err.Error()
		return
	}
	n.Meta = pluralize(len(assets), "asset")
	for _, a := range assets {
		meta := output.FormatSize(a.Size)
		if len(a.SHA256) >= 8 {
			meta += "  " + a.SHA256[:8]
		}
		n.Children = append(n.Children, &TreeNode{
			Name: a.Name,
			Kind: "asset",
			Path: fmt.Sprintf("%s/%s/%s/%s@%s/%s", c.Domain, c.Repository, c.Namespace, c.Package, c.Version, a.Name),
			Meta: meta,
		})
	}
}

// pluralize returns "1 word" or "N words" — the trivial English plural.
func pluralize(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// flattenLeaves walks the tree and returns every leaf's Path. A leaf is a
// node where the walk stopped (no children, by depth or by absence) and
// for which a Path was recorded — the root node has no Path and is
// skipped. Used by ls -R for its flat output; the order follows the walk.
func flattenLeaves(n *TreeNode) []string {
	var out []string
	var visit func(*TreeNode)
	visit = func(node *TreeNode) {
		if len(node.Children) == 0 {
			if node.Path != "" {
				out = append(out, node.Path)
			}
			return
		}
		for _, c := range node.Children {
			visit(c)
		}
	}
	visit(n)
	return out
}

// countErrors returns the number of nodes in the walked tree that recorded
// a listing error. Callers warn once with the count rather than rendering
// the same situation node-by-node in summary text.
func countErrors(n *TreeNode) int {
	c := 0
	if n.Error != "" {
		c++
	}
	for _, child := range n.Children {
		c += countErrors(child)
	}
	return c
}
