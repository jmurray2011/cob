package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	catypes "github.com/aws/aws-sdk-go-v2/service/codeartifact/types"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/output"
)

func TestParseTreeDepth(t *testing.T) {
	cases := []struct {
		in   string
		want TreeDepth
		err  bool
	}{
		{"", DepthPackages, false}, // default
		{"packages", DepthPackages, false},
		{"domains", DepthDomains, false},
		{"repos", DepthRepos, false},
		{"versions", DepthVersions, false},
		{"assets", DepthAssets, false},
		{"asset", 0, true},    // not aliased
		{"PACKAGES", 0, true}, // case-sensitive
	}
	for _, c := range cases {
		got, err := parseTreeDepth(c.in)
		if (err != nil) != c.err {
			t.Errorf("parseTreeDepth(%q) err=%v, wantErr=%v", c.in, err, c.err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("parseTreeDepth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestStartDepthOf(t *testing.T) {
	cases := []struct {
		c    *cob.PackageCoordinates
		want TreeDepth
	}{
		{nil, DepthDomains},
		{&cob.PackageCoordinates{}, DepthDomains},
		{&cob.PackageCoordinates{Domain: "d"}, DepthDomains},
		{&cob.PackageCoordinates{Domain: "d", Repository: "r"}, DepthRepos},
		{&cob.PackageCoordinates{Domain: "d", Repository: "r", Namespace: "n", Package: "p"}, DepthPackages},
		{&cob.PackageCoordinates{Domain: "d", Repository: "r", Namespace: "n", Package: "p", Version: "1.0.0"}, DepthVersions},
	}
	for i, c := range cases {
		if got := startDepthOf(c.c); got != c.want {
			t.Errorf("case %d: startDepthOf = %d, want %d", i, got, c.want)
		}
	}
}

func TestTreeLabel(t *testing.T) {
	cases := []struct {
		n    *TreeNode
		want string
	}{
		{&TreeNode{Name: "acme"}, "acme"},
		{&TreeNode{Name: "acme", Meta: "2 repos"}, "acme  (2 repos)"},
		{&TreeNode{Name: "acme", Error: "boom"}, "acme  ! boom"},
		// Error wins over meta — the failure is the most important fact.
		{&TreeNode{Name: "acme", Meta: "2 repos", Error: "boom"}, "acme  ! boom"},
	}
	for _, c := range cases {
		if got := treeLabel(c.n); got != c.want {
			t.Errorf("treeLabel(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestRenderSubtreeBoxDrawing(t *testing.T) {
	// A small two-deep tree exercises both connectors (├──, └──) and both
	// child prefixes (│  , spaces). A regression in either would corrupt
	// every other tree this command prints.
	root := &TreeNode{Name: "acme", Kind: "domain", Meta: "2 repos", Children: []*TreeNode{
		{Name: "dev", Kind: "repo", Meta: "1 package", Children: []*TreeNode{
			{Name: "tools/my-app", Kind: "package", Meta: "1 version, latest 2.1.0"},
		}},
		{Name: "prod", Kind: "repo", Meta: "0 packages"},
	}}
	var buf bytes.Buffer
	w := output.NewWithWriters(&buf, &buf, false)
	renderTreeText(w, root)

	got := buf.String()
	wantLines := []string{
		"acme  (2 repos)",
		"├── dev  (1 package)",
		"│   └── tools/my-app  (1 version, latest 2.1.0)",
		"└── prod  (0 packages)",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("renderTreeText missing line %q in:\n%s", line, got)
		}
	}
}

func TestFlattenLeaves(t *testing.T) {
	// A walked tree: root → 2 domains, one of which has 2 packages; the
	// other branch errored and has no children. Leaves are the package
	// paths (depth=packages) plus the errored domain (no children but no
	// path → skipped from the flat output).
	root := &TreeNode{Kind: "root", Children: []*TreeNode{
		{Name: "acme", Kind: "domain", Path: "acme", Children: []*TreeNode{
			{Name: "dev", Kind: "repo", Path: "acme/dev", Children: []*TreeNode{
				{Name: "ns/a", Kind: "package", Path: "acme/dev/ns/a"},
				{Name: "ns/b", Kind: "package", Path: "acme/dev/ns/b"},
			}},
		}},
		{Name: "broken", Kind: "domain", Path: "broken", Error: "denied"},
	}}
	got := flattenLeaves(root)
	want := []string{"acme/dev/ns/a", "acme/dev/ns/b", "broken"}
	if len(got) != len(want) {
		t.Fatalf("flattenLeaves = %v, want %v", got, want)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("flattenLeaves[%d] = %q, want %q", i, got[i], p)
		}
	}
}

func TestCountErrors(t *testing.T) {
	root := &TreeNode{Kind: "root", Children: []*TreeNode{
		{Name: "a", Error: "x"},
		{Name: "b", Children: []*TreeNode{
			{Name: "b1"},
			{Name: "b2", Error: "y"},
		}},
		{Name: "c"},
	}}
	if got := countErrors(root); got != 2 {
		t.Errorf("countErrors = %d, want 2", got)
	}
}

// treeFake builds a fakeCA wired to return a small fixed namespace:
//
//	acme/dev/tools/app  (one version 1.0.0)
//
// Each command goes through this against a hand-built fake, so the walker
// is exercised end-to-end without mocking the registry.
func treeFake() *fakeCA {
	return &fakeCA{
		listDomainsFn: func(*codeartifact.ListDomainsInput) (*codeartifact.ListDomainsOutput, error) {
			return &codeartifact.ListDomainsOutput{Domains: []catypes.DomainSummary{
				{Name: aws.String("acme"), Status: catypes.DomainStatusActive},
			}}, nil
		},
		listReposFn: func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error) {
			return &codeartifact.ListRepositoriesInDomainOutput{Repositories: []catypes.RepositorySummary{
				{Name: aws.String("dev")},
			}}, nil
		},
		listPackagesFn: func(*codeartifact.ListPackagesInput) (*codeartifact.ListPackagesOutput, error) {
			return &codeartifact.ListPackagesOutput{Packages: []catypes.PackageSummary{
				{Namespace: aws.String("tools"), Package: aws.String("app")},
			}}, nil
		},
		listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{
				Versions: []catypes.PackageVersionSummary{{Version: aws.String("1.0.0")}},
			}, nil
		},
		listAssetsFn: oneAsset("app.bin", 5),
	}
}

func TestRunTreeDefault(t *testing.T) {
	cfg, stdout, _ := useFake(t, treeFake())
	root := newTreeCmd(cfg)
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatalf("tree: %v", err)
	}
	out := stdout.String()
	// Walk reaches packages by default; the tree string must include every
	// level above and the package itself with its inline meta.
	for _, want := range []string{
		"acme",          // domain header
		"└── dev",       // last (only) repo
		"└── tools/app", // last (only) package
		"latest 1.0.0",  // meta from ListPackages
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tree output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunTreeDefaultBumpsBelowTarget(t *testing.T) {
	// Targeting a package with no explicit --depth should descend into
	// versions — otherwise the result is one node with no children, which
	// is useless and was the gotcha the bump exists to avoid.
	cfg, stdout, _ := useFake(t, treeFake())
	root := newTreeCmd(cfg)
	root.SetArgs([]string{"acme/dev/tools/app"})
	if err := root.Execute(); err != nil {
		t.Fatalf("tree: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "1.0.0") {
		t.Errorf("targeted-package walk should descend to versions; got:\n%s", out)
	}
}

func TestRunTreeRejectsShallowerDepth(t *testing.T) {
	cfg, _, _ := useFake(t, treeFake())
	root := newTreeCmd(cfg)
	// Explicit shallower --depth contradicts the target; tree refuses
	// rather than silently producing nothing.
	root.SetArgs([]string{"acme/dev", "--depth", "domains"})
	err := root.Execute()
	wantExit(t, err, cob.ExitError)
}

func TestRunTreeJSON(t *testing.T) {
	cfg, stdout, _ := useFake(t, treeFake())
	cfg.JSON = true
	root := newTreeCmd(cfg)
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatalf("tree --json: %v", err)
	}
	var n TreeNode
	if err := json.Unmarshal(stdout.Bytes(), &n); err != nil {
		t.Fatalf("tree --json emitted invalid JSON: %v\n%s", err, stdout.String())
	}
	if n.Kind != "root" {
		t.Errorf("JSON tree root kind = %q, want root", n.Kind)
	}
	if len(n.Children) != 1 || n.Children[0].Name != "acme" {
		t.Errorf("JSON tree shape wrong: %+v", n)
	}
}

func TestRunLsRecursiveFlat(t *testing.T) {
	cfg, stdout, _ := useFake(t, treeFake())
	root := newLsCmd(cfg)
	root.SetArgs([]string{"-R"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ls -R: %v", err)
	}
	got := strings.TrimSpace(stdout.String())
	if got != "acme/dev/tools/app" {
		t.Errorf("ls -R = %q, want acme/dev/tools/app", got)
	}
}

func TestRunLsRecursiveDepthVersions(t *testing.T) {
	cfg, stdout, _ := useFake(t, treeFake())
	root := newLsCmd(cfg)
	root.SetArgs([]string{"-R", "--depth", "versions"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ls -R --depth versions: %v", err)
	}
	got := strings.TrimSpace(stdout.String())
	want := "acme/dev/tools/app@1.0.0"
	if got != want {
		t.Errorf("ls -R --depth versions = %q, want %q", got, want)
	}
}

func TestRunLsRecursiveJSON(t *testing.T) {
	cfg, stdout, _ := useFake(t, treeFake())
	cfg.JSON = true
	root := newLsCmd(cfg)
	root.SetArgs([]string{"-R"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ls -R --json: %v", err)
	}
	var paths []string
	if err := json.Unmarshal(stdout.Bytes(), &paths); err != nil {
		t.Fatalf("ls -R --json emitted invalid JSON: %v\n%s", err, stdout.String())
	}
	if len(paths) != 1 || paths[0] != "acme/dev/tools/app" {
		t.Errorf("ls -R --json = %v, want [acme/dev/tools/app]", paths)
	}
}

func TestRunLsRecursiveEmptyIsNotFound(t *testing.T) {
	// No domains -> nothing to walk -> ExitNotFound + (in JSON mode) an
	// empty array, matching the rest of ls.
	cfg, _, _ := useFake(t, &fakeCA{}) // defaults: every list returns empty
	root := newLsCmd(cfg)
	root.SetArgs([]string{"-R"})
	wantExit(t, root.Execute(), cob.ExitNotFound)
}

func TestWalkRecordsPerBranchErrors(t *testing.T) {
	// One repo's ListPackages errors; the rest of the tree should still
	// resolve and the failure should be visible on its node, not fatal.
	ca := &fakeCA{
		listDomainsFn: func(*codeartifact.ListDomainsInput) (*codeartifact.ListDomainsOutput, error) {
			return &codeartifact.ListDomainsOutput{Domains: []catypes.DomainSummary{
				{Name: aws.String("acme")},
			}}, nil
		},
		listReposFn: func(*codeartifact.ListRepositoriesInDomainInput) (*codeartifact.ListRepositoriesInDomainOutput, error) {
			return &codeartifact.ListRepositoriesInDomainOutput{Repositories: []catypes.RepositorySummary{
				{Name: aws.String("good")},
				{Name: aws.String("bad")},
			}}, nil
		},
		listPackagesFn: func(in *codeartifact.ListPackagesInput) (*codeartifact.ListPackagesOutput, error) {
			if aws.ToString(in.Repository) == "bad" {
				return nil, &catypes.AccessDeniedException{Message: aws.String("nope")}
			}
			return &codeartifact.ListPackagesOutput{Packages: []catypes.PackageSummary{
				{Namespace: aws.String("ns"), Package: aws.String("good-pkg")},
			}}, nil
		},
		listVersionsFn: func(*codeartifact.ListPackageVersionsInput) (*codeartifact.ListPackageVersionsOutput, error) {
			return &codeartifact.ListPackageVersionsOutput{
				Versions: []catypes.PackageVersionSummary{{Version: aws.String("1")}},
			}, nil
		},
	}
	cfg, stdout, stderr := useFake(t, ca)
	root := newTreeCmd(cfg)
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatalf("tree with one bad branch should not error overall: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "good-pkg") {
		t.Errorf("good branch must still appear: %s", out)
	}
	if !strings.Contains(out, "bad") || !strings.Contains(out, "!") {
		t.Errorf("bad branch should be marked with an inline error: %s", out)
	}
	if !strings.Contains(stderr.String(), "could not be listed") {
		t.Errorf("expected an error-count warning on stderr: %s", stderr.String())
	}
}
