package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/concurrency"
	"github.com/jmurray2011/cob/internal/output"
)

// treeWalkConcurrency bounds both the in-flight CodeArtifact list calls and
// the goroutines a hierarchy walk holds at once. Matches
// cliutil.PromotionStatusConcurrency — both are batches of independent reads
// against the same service.
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
	w := &walker{registry: registry, depth: depth}
	root := startNode(start)
	w.walk(ctx, root)
	return root
}

type walker struct {
	registry *cob.Registry
	depth    TreeDepth
}

// startNode builds the unexpanded root of the walk from the start
// coordinates: every level of detail that was supplied pins one more node
// kind. nil/empty start means a whole-namespace walk rooted at "root".
func startNode(start *cob.PackageCoordinates) *TreeNode {
	switch {
	case start == nil || start.Domain == "":
		return &TreeNode{Kind: "root"}
	case start.Repository == "":
		return &TreeNode{Name: start.Domain, Kind: "domain", Path: start.Domain, Domain: start.Domain}
	case start.Namespace == "":
		return &TreeNode{
			Name: start.Repository, Kind: "repo",
			Path:   start.Domain + "/" + start.Repository,
			Domain: start.Domain, Repository: start.Repository,
		}
	case start.Version == "":
		return &TreeNode{
			Name: start.Namespace + "/" + start.Package, Kind: "package",
			Path:   fmt.Sprintf("%s/%s/%s/%s", start.Domain, start.Repository, start.Namespace, start.Package),
			Domain: start.Domain, Repository: start.Repository,
			Namespace: start.Namespace, Package: start.Package,
		}
	default:
		return &TreeNode{
			Name: start.Version, Kind: "version",
			Path:   fmt.Sprintf("%s/%s/%s/%s@%s", start.Domain, start.Repository, start.Namespace, start.Package, start.Version),
			Domain: start.Domain, Repository: start.Repository,
			Namespace: start.Namespace, Package: start.Package, Version: start.Version,
		}
	}
}

// levelOf maps a node kind to its absolute depth. The synthetic "root" is
// above DepthDomains so it always expands into the domain list.
func levelOf(kind string) TreeDepth {
	switch kind {
	case "domain":
		return DepthDomains
	case "repo":
		return DepthRepos
	case "package":
		return DepthPackages
	case "version":
		return DepthVersions
	case "asset":
		return DepthAssets
	default: // root
		return DepthDomains - 1
	}
}

// walk expands the tree breadth-first, one level at a time. Each level's
// nodes are expanded through concurrency.ForEach, which acquires a
// treeWalkConcurrency-slot semaphore *before* spawning a goroutine — so the
// walk holds at most treeWalkConcurrency goroutines (and in-flight API
// calls) at once, regardless of how wide the tree is. A node is expanded
// only while its level is shallower than the target depth; deeper nodes are
// leaves. Per-branch errors land on the node (expandNode), so one bad branch
// never aborts the walk.
func (w *walker) walk(ctx context.Context, root *TreeNode) {
	frontier := []*TreeNode{root}
	for len(frontier) > 0 {
		var expandable []*TreeNode
		for _, n := range frontier {
			if levelOf(n.Kind) < w.depth {
				expandable = append(expandable, n)
			}
		}
		if len(expandable) == 0 {
			return
		}
		concurrency.ForEach(ctx, expandable, treeWalkConcurrency, func(ctx context.Context, _ int, n *TreeNode) struct{} {
			w.expandNode(ctx, n)
			return struct{}{}
		})
		var next []*TreeNode
		for _, n := range expandable {
			next = append(next, n.Children...)
		}
		frontier = next
	}
}

// expandNode lists n's immediate children for its kind. No throttling or
// goroutine spawning here — walk's ForEach owns concurrency; each call is a
// plain, synchronous listing run on a bounded worker.
func (w *walker) expandNode(ctx context.Context, n *TreeNode) {
	switch n.Kind {
	case "root":
		w.expandRoot(ctx, n)
	case "domain":
		w.expandDomain(ctx, n)
	case "repo":
		w.expandRepo(ctx, n)
	case "package":
		w.expandPackage(ctx, n)
	case "version":
		w.expandVersion(ctx, n)
	}
}

func (w *walker) expandRoot(ctx context.Context, n *TreeNode) {
	domains, err := w.registry.ListDomains(ctx)
	if err != nil {
		n.Error = err.Error()
		return
	}
	for _, d := range domains {
		n.Children = append(n.Children, &TreeNode{Name: d.Name, Kind: "domain", Path: d.Name, Domain: d.Name})
	}
}

func (w *walker) expandDomain(ctx context.Context, n *TreeNode) {
	repos, err := w.registry.ListRepositories(ctx, n.Domain)
	if err != nil {
		n.Error = err.Error()
		return
	}
	n.RepoCount = len(repos)
	for _, r := range repos {
		n.Children = append(n.Children, &TreeNode{
			Name: r, Kind: "repo", Path: n.Domain + "/" + r,
			Domain: n.Domain, Repository: r,
		})
	}
}

func (w *walker) expandRepo(ctx context.Context, n *TreeNode) {
	pkgs, err := w.registry.ListPackages(ctx, n.Domain, n.Repository)
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
			Path:   fmt.Sprintf("%s/%s/%s/%s", n.Domain, n.Repository, p.Namespace, p.Package),
			Domain: n.Domain, Repository: n.Repository,
			Namespace: p.Namespace, Package: p.Package,
			VersionCount:  p.VersionCount,
			LatestVersion: p.LatestVersion,
		})
	}
}

func (w *walker) expandPackage(ctx context.Context, n *TreeNode) {
	c := cob.PackageCoordinates{Domain: n.Domain, Repository: n.Repository, Namespace: n.Namespace, Package: n.Package}
	versions, err := w.registry.ListVersions(ctx, &c)
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
}

func (w *walker) expandVersion(ctx context.Context, n *TreeNode) {
	c := cob.PackageCoordinates{Domain: n.Domain, Repository: n.Repository, Namespace: n.Namespace, Package: n.Package, Version: n.Version}
	assets, err := w.registry.ListAssets(ctx, &c)
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
