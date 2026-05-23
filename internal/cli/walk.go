package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

// treeWalkConcurrency bounds in-flight CodeArtifact list calls during a
// hierarchy walk. Matches cliutil.PromotionStatusConcurrency — both are batches of
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

// TreeNode is one node of the walked hierarchy, internal to the walker and
// text renderer. Kind names the level; Path is the fully-qualified
// coordinate string; the typed payload fields are populated according to
// Kind (e.g. a "package" node carries Namespace/Package/VersionCount/
// LatestVersion; a "version" node carries Version/AssetCount/Published).
// Children is nil when the walk stopped at this node (by depth or by
// absence). Error captures a per-node listing failure so one bad branch
// doesn't kill an org-wide discovery — the rest of the tree is still
// useful. The structured fields are also what the JSON output emits (in
// flat form via FlatNode) — both renderers read the same source of truth,
// so the text label and the JSON record can never disagree.
type TreeNode struct {
	Name string // display segment (last component): "acme", "tools/api", "2.1.0", "app.bin"
	Kind string // root | domain | repo | package | version | asset
	Path string // canonical coordinate, e.g. "acme/dev/tools/api@2.1.0/app.bin"

	// Coordinate components, filled in as the walk descends.
	Domain     string
	Repository string
	Namespace  string
	Package    string
	Version    string
	Asset      string

	// Per-kind payload. Each is set when the relevant listing populated it.
	RepoCount     int    // domain
	PackageCount  int    // repo
	VersionCount  int    // package
	LatestVersion string // package
	AssetCount    int    // version
	Published     string // version (RFC 3339)
	Size          int64  // asset
	SHA256        string // asset

	Children []*TreeNode
	Error    string
}

// FlatNode is the JSON record shape: one per node in walk order, no
// nesting, typed payload fields so a jq consumer can filter on
// version_count or latest_version without parsing strings. omitempty drops
// per-kind fields that don't apply, so a domain record doesn't carry an
// empty "asset"/"size"/"sha256" — only kind, path, the populated coord
// components, and the relevant payload.
type FlatNode struct {
	Path string `json:"path"`
	Kind string `json:"kind"`

	Domain     string `json:"domain,omitempty"`
	Repository string `json:"repository,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Package    string `json:"package,omitempty"`
	Version    string `json:"version,omitempty"`
	Asset      string `json:"asset,omitempty"`

	RepoCount     int    `json:"repo_count,omitempty"`
	PackageCount  int    `json:"package_count,omitempty"`
	VersionCount  int    `json:"version_count,omitempty"`
	LatestVersion string `json:"latest_version,omitempty"`
	AssetCount    int    `json:"asset_count,omitempty"`
	Published     string `json:"published,omitempty"`
	Size          int64  `json:"size,omitempty"`
	SHA256        string `json:"sha256,omitempty"`

	Error string `json:"error,omitempty"`
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
		n := &TreeNode{Name: start.Domain, Kind: "domain", Path: start.Domain, Domain: start.Domain}
		if depth >= DepthRepos {
			w.expandDomain(ctx, n, start.Domain)
		}
		return n
	case start.Namespace == "":
		n := &TreeNode{
			Name: start.Repository, Kind: "repo",
			Path:   start.Domain + "/" + start.Repository,
			Domain: start.Domain, Repository: start.Repository,
		}
		if depth >= DepthPackages {
			w.expandRepo(ctx, n, start.Domain, start.Repository)
		}
		return n
	case start.Version == "":
		path := fmt.Sprintf("%s/%s/%s/%s", start.Domain, start.Repository, start.Namespace, start.Package)
		n := &TreeNode{
			Name: start.Namespace + "/" + start.Package, Kind: "package", Path: path,
			Domain: start.Domain, Repository: start.Repository,
			Namespace: start.Namespace, Package: start.Package,
		}
		if depth >= DepthVersions {
			w.expandPackage(ctx, n, *start)
		}
		return n
	default:
		path := fmt.Sprintf("%s/%s/%s/%s@%s", start.Domain, start.Repository, start.Namespace, start.Package, start.Version)
		n := &TreeNode{
			Name: start.Version, Kind: "version", Path: path,
			Domain: start.Domain, Repository: start.Repository,
			Namespace: start.Namespace, Package: start.Package, Version: start.Version,
		}
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
	for _, d := range domains {
		n.Children = append(n.Children, &TreeNode{Name: d.Name, Kind: "domain", Path: d.Name, Domain: d.Name})
	}
	if w.depth <= DepthDomains {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		w.expandDomain(ctx, child, child.Domain)
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
	n.RepoCount = len(repos)
	for _, r := range repos {
		n.Children = append(n.Children, &TreeNode{
			Name: r, Kind: "repo", Path: domain + "/" + r,
			Domain: domain, Repository: r,
		})
	}
	if w.depth <= DepthRepos {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		w.expandRepo(ctx, child, domain, child.Repository)
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
	n.PackageCount = len(pkgs)
	for _, p := range pkgs {
		// PackageSummary already carries LatestVersion + VersionCount from
		// ListPackages, so the package row gets its summary at no extra API
		// cost — this is what makes depth=packages cheap.
		n.Children = append(n.Children, &TreeNode{
			Name:   p.Namespace + "/" + p.Package,
			Kind:   "package",
			Path:   fmt.Sprintf("%s/%s/%s/%s", domain, repo, p.Namespace, p.Package),
			Domain: domain, Repository: repo,
			Namespace: p.Namespace, Package: p.Package,
			VersionCount:  p.VersionCount,
			LatestVersion: p.LatestVersion,
		})
	}
	if w.depth <= DepthPackages {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		w.expandPackage(ctx, child, cob.PackageCoordinates{
			Domain: domain, Repository: repo,
			Namespace: child.Namespace, Package: child.Package,
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
	// Refresh the package's own version count from the actual listing — the
	// VersionCount that came from ListPackages can be stale by a moment.
	n.VersionCount = len(versions)
	for _, v := range versions {
		published := ""
		if !v.Published.IsZero() {
			published = v.Published.UTC().Format(time.RFC3339)
		}
		n.Children = append(n.Children, &TreeNode{
			Name: v.Version, Kind: "version",
			Path:   fmt.Sprintf("%s/%s/%s/%s@%s", c.Domain, c.Repository, c.Namespace, c.Package, v.Version),
			Domain: c.Domain, Repository: c.Repository,
			Namespace: c.Namespace, Package: c.Package, Version: v.Version,
			AssetCount: v.Assets, Published: published,
		})
	}
	if w.depth <= DepthVersions {
		return
	}
	w.recurse(ctx, n.Children, func(ctx context.Context, child *TreeNode) {
		vc := c
		vc.Version = child.Version
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
	n.AssetCount = len(assets)
	for _, a := range assets {
		n.Children = append(n.Children, &TreeNode{
			Name: a.Name, Kind: "asset",
			Path:   fmt.Sprintf("%s/%s/%s/%s@%s/%s", c.Domain, c.Repository, c.Namespace, c.Package, c.Version, a.Name),
			Domain: c.Domain, Repository: c.Repository,
			Namespace: c.Namespace, Package: c.Package, Version: c.Version,
			Asset:  a.Name,
			Size:   a.Size,
			SHA256: a.SHA256,
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

// labelMeta renders the human-readable summary that follows a node's name
// in tree mode. It reads typed fields off the node — the same fields the
// JSON output marshals — so the two views can't disagree.
func labelMeta(n *TreeNode) string {
	switch n.Kind {
	case "domain":
		if n.Children == nil {
			return ""
		}
		return pluralize(n.RepoCount, "repo")
	case "repo":
		if n.Children == nil {
			return ""
		}
		return pluralize(n.PackageCount, "package")
	case "package":
		switch {
		case n.VersionCount == 0:
			return "no versions"
		case n.LatestVersion == "":
			return pluralize(n.VersionCount, "version")
		default:
			return fmt.Sprintf("%s, latest %s", pluralize(n.VersionCount, "version"), n.LatestVersion)
		}
	case "version":
		meta := pluralize(n.AssetCount, "asset")
		if n.Published != "" {
			if t, err := time.Parse(time.RFC3339, n.Published); err == nil {
				meta += ", " + t.Format("2006-01-02")
			}
		}
		return meta
	case "asset":
		meta := output.FormatSize(n.Size)
		if len(n.SHA256) >= 8 {
			meta += "  " + n.SHA256[:8]
		}
		return meta
	}
	return ""
}

// flattenForJSON walks the tree in pre-order and returns one FlatNode per
// node, skipping the synthetic "root". The result is what tree --json
// emits: a flat array, each record self-contained, jq-filterable on typed
// fields rather than on substrings of a free-form "meta" string.
func flattenForJSON(root *TreeNode) []FlatNode {
	var out []FlatNode
	var visit func(*TreeNode)
	visit = func(n *TreeNode) {
		if n.Kind != "root" {
			out = append(out, FlatNode{
				Path:          n.Path,
				Kind:          n.Kind,
				Domain:        n.Domain,
				Repository:    n.Repository,
				Namespace:     n.Namespace,
				Package:       n.Package,
				Version:       n.Version,
				Asset:         n.Asset,
				RepoCount:     n.RepoCount,
				PackageCount:  n.PackageCount,
				VersionCount:  n.VersionCount,
				LatestVersion: n.LatestVersion,
				AssetCount:    n.AssetCount,
				Published:     n.Published,
				Size:          n.Size,
				SHA256:        n.SHA256,
				Error:         n.Error,
			})
		}
		for _, c := range n.Children {
			visit(c)
		}
	}
	visit(root)
	return out
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
