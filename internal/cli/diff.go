package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

// diff is cob's only comparison verb. Mode is picked from the
// positional shape — there's no separate `validate`, no separate
// `verify`; lint and integrity checks are diff cases. The shared
// renderers live in diff_render.go and the validation helper in
// validation.go runs implicitly inside manifest mode (and at the top
// of any other manifest-consuming command) so a broken manifest
// short-circuits before any AWS work.
//
//	cob diff <manifest>                  → lint only (offline; was `cob validate`)
//	cob diff <manifest> --version X      → manifest sources vs published @X
//	cob diff <coords>                    → self-integrity: provenance vs live
//	cob diff <dir> <coords>              → local files vs published
//	cob diff <coords-A> <coords-B>       → version vs version
func newDiffCmd(cfg *Config) *cobra.Command {
	var (
		flagVersion string
		flagDeep    bool
		flagVerbose bool
	)

	cmd := &cobra.Command{
		Use:   "diff <target> [<target>]",
		Short: "Compare bytes — manifest vs published, version vs version, local dir vs published, or lint a manifest",
		Long: "diff is cob's only comparison verb. The mode is picked from the " +
			"positional argument(s); none mutate; exit code is 0 if identical, " +
			"non-zero on any drift or error.\n\n" +
			"Five modes:\n\n" +
			"1. Manifest lint (one arg, file ending .yaml/.yml, no --version): " +
			"schema + URI syntax + local-file existence. Offline. The same " +
			"checks run implicitly at the top of every other manifest-based " +
			"command, so this mode is just \"give me the lint result.\"\n\n" +
			"2. Manifest vs published (one arg, file ending .yaml/.yml, with " +
			"--version): hashes each source and compares to the published " +
			"asset of the same name. Precedence: known checksum → recorded S3 " +
			"origin → cob-provenance.json → --deep download+hash.\n\n" +
			"3. Self-integrity (one arg, coordinates with version): fetches the " +
			"recorded cob-provenance.json and compares each entry's SHA-256 " +
			"to what CodeArtifact currently stores. Prints the chain of " +
			"evidence first. Audit a version with nothing but its coordinates.\n\n" +
			"4. Local dir vs published (two args, first must be a directory): " +
			"for each published asset of <coords>, looks for a local file of " +
			"the same name in <dir> and compares SHA-256. Answers \"do these " +
			"local files match what was published?\"\n\n" +
			"5. Version vs version (two args, both coordinates): compares two " +
			"published versions of the same package. cob-provenance.json is " +
			"excluded (its bytes trivially differ even when the package " +
			"didn't change). Cross-repo same-package is allowed (\"did the " +
			"promote preserve the bytes?\").",
		Example: `  # 1. Lint a manifest (offline)
  cob diff ./my-package.yaml

  # 2. Manifest vs published
  cob diff ./my-package.yaml --version 2.1.0

  # 3. Self-integrity (audit by coordinates)
  cob diff acme/dev/tools/my-app@2.1.0

  # 4. Local dir vs published
  cob diff ~/pulled-dir acme/dev/tools/my-app@2.1.0

  # 5. Version vs version
  cob diff acme/dev/tools/my-app@2.0.0 acme/dev/tools/my-app@2.1.0`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd.Context(), cfg, args, flagVersion, flagDeep, flagVerbose)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (manifest mode with --version → online; without → offline lint)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Manifest mode: download and hash sources lacking a checksum (no S3 writes)")
	cmd.Flags().BoolVarP(&flagVerbose, "verbose", "v", false, "Show source URI and full SHA-256 on every row (default: only on mismatches)")
	return cmd
}

// runDiff routes to the right mode by inspecting positional shape and
// filesystem state. Each branch's discriminator is explicit so a typo
// surfaces as an error pointing at the right invocation, not as a
// silent fallback into the wrong mode.
func runDiff(ctx context.Context, cfg *Config, args []string, versionFlag string, deep, verbose bool) error {
	out := newWriter(cfg)
	defer out.Close()

	switch len(args) {
	case 1:
		target := args[0]
		if isManifestPath(target) {
			version := versionFlag
			if version == "" {
				version = os.Getenv("COB_VERSION")
			}
			if version == "" {
				// No --version → offline lint. The implicit validation
				// also runs at the top of every other manifest-based
				// command; this mode is the user-facing report of it.
				return runDiffLint(out, target)
			}
			return runDiffManifest(ctx, cfg, out, target, version, deep, verbose)
		}
		// Directory as a single arg is ambiguous (which package?) — point
		// at the right shape instead of guessing.
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			return fail(out, "diff", cob.ExitError,
				"%s is a directory — to diff its files against a published version, give the coordinates as a second argument:\n  cob diff %s <domain>/<repo>/<ns>/<pkg>@<version>",
				target, target)
		}
		// Otherwise: coordinates → self-integrity check.
		return runDiffSelfCheck(ctx, cfg, out, target, verbose)
	case 2:
		// Two args: dir + coords, OR coords + coords.
		info, err := os.Stat(args[0])
		if err == nil && info.IsDir() {
			return runDiffDir(ctx, cfg, out, args[0], args[1], verbose)
		}
		// Reject "file + coords" — the only valid first-arg-is-file case
		// is manifest mode, which takes ONE positional. A two-arg invocation
		// where the first isn't a directory is a typo.
		if err == nil {
			return fail(out, "diff", cob.ExitError,
				"first argument must be a directory or coordinates when two args are given; got file %s", args[0])
		}
		return runDiffVersions(ctx, cfg, out, args[0], args[1], verbose)
	}
	return fail(out, "diff", cob.ExitError, "diff takes one or two positional arguments")
}

// runDiffLint runs the offline checks the old `cob validate` exposed.
// Same per-source rendering — labels each line with what was actually
// checked (local existence vs remote syntax-only). The underlying
// validateManifest helper is the same one publish/promote/pull invoke
// implicitly, so "lint says it's OK" and "implicit pre-flight said
// nothing" can never disagree.
func runDiffLint(out *output.Writer, manifestPath string) error {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)

	version := os.Getenv("COB_VERSION")
	versionResolved := version != ""
	if !versionResolved {
		version = "0.0.0-validate"
	}

	out.Header("Linting %s", manifestPath)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s", m.Namespace, m.Package),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}

	var failures, localOK, remoteOK int
	byAsset := make(map[string]string, len(m.Sources))
	for _, s := range m.Sources {
		ar := cob.AssetResult{Name: s.Name, Source: s.URI, Method: "syntax"}

		resolved, err := manifest.ExpandURI(s.URI, version)
		if err != nil {
			ar.SetError(err)
			out.Plain("  ✗ %s  %s", s.Name, err)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Source = resolved

		asset, size, kind, err := validateSourceURI(resolved, m.Dir)
		if err != nil {
			ar.SetError(err)
			out.Plain("  ✗ %s  %s", s.Name, err)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Size = size
		if asset == cob.ProvenanceFile {
			e := fmt.Errorf("uses the reserved asset name %q (cob writes that as the publish finalizer)", asset)
			ar.SetError(e)
			out.Plain("  ✗ %s  %s", s.Name, e)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		if prev, dup := byAsset[asset]; dup {
			e := fmt.Errorf("collides with source %q: both publish as asset %q", prev, asset)
			ar.SetError(e)
			out.Plain("  ✗ %s  %s", s.Name, e)
			failures++
			result.Assets = append(result.Assets, ar)
			continue
		}
		byAsset[asset] = s.Name

		switch kind {
		case uriFile:
			ar.Method = "exists"
			localOK++
			out.Plain("  ✓ %s  %s  (%s)", s.Name, resolved, output.FormatSize(size))
		case uriS3:
			ar.Method = "syntax(s3)"
			remoteOK++
			out.Plain("  ✓ %s  %s  (remote, syntax only)", s.Name, resolved)
		case uriCA:
			ar.Method = "syntax(ca)"
			remoteOK++
			out.Plain("  ✓ %s  %s  (remote, syntax only)", s.Name, resolved)
		}
		result.Assets = append(result.Assets, ar)
	}

	if !versionResolved {
		out.Warn("no COB_VERSION; ${VERSION} validated structurally only")
	}

	if failures > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d of %d sources invalid", failures, len(m.Sources))
		out.Summary("Invalid: %d of %d sources failed.", failures, len(m.Sources))
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}

	switch {
	case localOK > 0 && remoteOK > 0:
		out.Summary("Lint OK: %d sources — %d local files verified to exist, %d remote URIs syntax-only. %d-stage promote pipeline.",
			len(m.Sources), localOK, remoteOK, promoteStageCount(m))
	case localOK > 0:
		out.Summary("Lint OK: %d sources — %d local files verified to exist. %d-stage promote pipeline.",
			len(m.Sources), localOK, promoteStageCount(m))
	case remoteOK > 0:
		out.Summary("Lint OK: %d sources — all remote URIs, syntax-only (pass --version to diff bytes against a published version). %d-stage promote pipeline.",
			len(m.Sources), promoteStageCount(m))
	default:
		out.Summary("Lint OK: %d sources, %d-stage promote pipeline.", len(m.Sources), promoteStageCount(m))
	}
	return out.CommandResult(result)
}

// runDiffManifest compares a manifest's sources to a published
// version's assets. Implicit validateManifest runs at the top — so a
// broken manifest can't reach the comparison loop.
func runDiffManifest(ctx context.Context, cfg *Config, out *output.Writer, manifestPath, version string, deep, verbose bool) error {
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)
	if err := validateManifest(m, version); err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	coords := &cob.PackageCoordinates{
		Domain: m.Domain, Repository: m.Repository,
		Namespace: m.Namespace, Package: m.Package, Version: version,
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	// Resolve @latest before expanding ${VERSION} — otherwise the
	// manifest's source URIs and the lookup would target a version
	// literally "latest".
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}
	version = coords.Version

	if err := m.ResolveVariables(version); err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	sources, err := buildSources(m, client)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}

	prov, perr := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if perr != nil {
		out.Warn("could not read %s: %s", cob.ProvenanceFile, perr)
	}
	cmps, err := compareManifestToPublished(ctx, sources, registry, coords, deep, prov)
	if err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}

	// Pub size lookup so mismatch rows show both sides' sizes when known.
	pubSize := make(map[string]int64)
	if pub, perr := registry.ListAssets(ctx, coords); perr == nil {
		for _, a := range pub {
			pubSize[a.Name] = a.Size
		}
	}

	out.Header("diff %s/%s@%s vs %s", m.Namespace, m.Package, version, manifestPath)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	names := make([]string, 0, len(cmps))
	for _, c := range cmps {
		names = append(names, c.Name)
	}
	nameWidth := pickNameColumnWidth(names)
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		m.Domain, m.Repository, m.Namespace, m.Package, version)

	var added, removed, changed, same, unknown, errs int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Source: c.Source, SHA256: c.SrcSHA}
		switch {
		case c.Err != nil:
			errs++
			ar.SetError(c.Err)
			renderDiffSingleFail(out, c.Name, "could not check", nameWidth,
				[]diffDetail{
					{"source", c.Source},
					{"error", c.Err.Error()},
				}, termWidth)
		case c.InManifest && !c.InPublished:
			added++
			ar.Method = "added"
			out.Plain("  + %s  (not published in @%s)", padRight(c.Name, nameWidth), version)
		case !c.InManifest && c.InPublished:
			removed++
			ar.Method = "removed"
			out.Plain("  - %s  (published, not in manifest)", padRight(c.Name, nameWidth))
		case c.OriginDrift:
			changed++
			ar.Method = "changed"
			renderDiffSingleFail(out, c.Name,
				"S3 source changed since publish (etag/version differs from provenance)",
				nameWidth, []diffDetail{{"source", c.Source}}, termWidth)
		case c.SrcSHA == "":
			unknown++
			ar.Method = "unknown"
			renderDiffSkipped(out, c.Name,
				"unverified — no checksum/provenance (use --deep to hash)",
				nameWidth, []diffDetail{{"source", c.Source}})
		case strings.EqualFold(c.SrcSHA, c.PubSHA):
			// hex SHA-256 case-insensitive — different sources/SDKs return
			// upper- vs lower-hex; EqualFold avoids a spurious mismatch.
			same++
			ar.Method = "match(" + c.SrcFrom + ")"
			renderDiffMatch(out, c.Name, c.Source,
				fmt.Sprintf("match(%s)", c.SrcFrom), c.SrcSHA,
				pubSize[c.Name], nameWidth, termWidth, verbose)
		default:
			changed++
			ar.Method = "changed"
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: source=%s published=%s", c.SrcSHA, c.PubSHA)
			renderDiffMismatch(out, c.Name, nameWidth,
				diffSide{label: "source", uri: c.Source, hash: c.SrcSHA},
				diffSide{label: "published", uri: pubLabel, size: pubSize[c.Name], hash: c.PubSHA},
			)
		}
		result.Assets = append(result.Assets, ar)
	}

	drift := added + removed + changed
	out.Summary("%d added, %d removed, %d changed, %d same, %d unknown.",
		added, removed, changed, same, unknown)

	if errs > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d source(s) could not be resolved", errs)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}
	if drift > 0 {
		result.Status = "drift"
		result.Error = fmt.Sprintf("%d added, %d removed, %d changed", added, removed, changed)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	return out.CommandResult(result)
}

// runDiffSelfCheck compares a published version's recorded provenance
// to what CodeArtifact currently stores. Was `cob verify <coords>`.
// Chain of evidence is printed first; the comparison follows.
func runDiffSelfCheck(ctx context.Context, cfg *Config, out *output.Writer, target string, verbose bool) error {
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "diff", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		return fail(out, "diff", cob.ExitError,
			"version required (use @version or @latest)")
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}

	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	if prov == nil {
		return fail(out, "diff", cob.ExitError,
			"no %s for %s/%s@%s — cannot self-check (was it published with cob?)",
			cob.ProvenanceFile, coords.Namespace, coords.Package, coords.Version)
	}

	assets, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}
	pubSHA := make(map[string]string, len(assets))
	pubSize := make(map[string]int64, len(assets))
	for _, a := range assets {
		pubSHA[a.Name] = a.SHA256
		pubSize[a.Name] = a.Size
	}

	out.Header("Self-check %s/%s@%s in %s/%s against recorded provenance",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)
	renderChain(out, prov, nil)
	renderOrigins(out, prov, "")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	names := make([]string, 0, len(prov.Assets))
	for _, e := range prov.Assets {
		names = append(names, e.Asset)
	}
	nameWidth := pickNameColumnWidth(names)
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)

	var failures int
	for _, e := range prov.Assets {
		ar := cob.AssetResult{Name: e.Asset, Source: e.Source, SHA256: e.SHA256, Size: e.Size}
		cur, ok := pubSHA[e.Asset]
		switch {
		case !ok:
			failures++
			ar.Method = "missing"
			renderDiffSingleFail(out, e.Asset,
				"recorded in provenance but not in the published version", nameWidth,
				[]diffDetail{
					{"recorded", fmt.Sprintf("%s   %s", rightPadSize(e.Size, 10), e.SHA256)},
				}, termWidth)
		case strings.EqualFold(cur, e.SHA256):
			ar.Method = "match(provenance)"
			renderDiffMatch(out, e.Asset, "", "match(provenance)", e.SHA256,
				e.Size, nameWidth, termWidth, verbose)
		default:
			failures++
			ar.Method = "altered"
			ar.ErrorMsg = fmt.Sprintf("altered since publish: recorded=%s published=%s", e.SHA256, cur)
			renderDiffMismatch(out, e.Asset, nameWidth,
				diffSide{label: "recorded", uri: "(provenance)", size: e.Size, hash: e.SHA256},
				diffSide{label: "published", uri: pubLabel, size: pubSize[e.Asset], hash: cur},
			)
		}
		result.Assets = append(result.Assets, ar)
	}

	out.Plain("")

	recorded := make(map[string]bool, len(prov.Assets))
	for _, e := range prov.Assets {
		recorded[e.Asset] = true
	}
	var extra int
	for _, a := range assets {
		if a.Name != cob.ProvenanceFile && !recorded[a.Name] {
			extra++
		}
	}
	if extra > 0 {
		out.Warn("%d published asset(s) not recorded in provenance", extra)
	}

	if failures > 0 {
		result.Status = "mismatch"
		result.Error = fmt.Sprintf("%d asset(s) altered or missing since publish", failures)
		out.Summary("FAILED: %d asset(s) altered or missing since publish.", failures)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	out.Summary("OK: every recorded asset still matches; chain intact.")
	return out.CommandResult(result)
}

// runDiffDir hashes every file in <dir> whose name matches a published
// asset of <coords> and compares to the published SHA-256. Was
// `cob verify <dir> <coords>`. Local files without a matching
// published asset are silently left alone (could be a README, source
// files, etc.). Published assets without a local file are reported as
// "missing locally". The cob-provenance.json asset is excluded — it's
// audit metadata, not a package file.
func runDiffDir(ctx context.Context, cfg *Config, out *output.Writer, dirPath, coordsArg string, verbose bool) error {
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	coords, err := manifest.ParseCoordinates(coordsArg)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "diff", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version]); got %q", coordsArg)
	}
	if coords.Version == "" {
		return fail(out, "diff", cob.ExitError,
			"version required: use @version or @latest in the coordinates")
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}

	pub, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}

	absDir, _ := filepath.Abs(dirPath)
	out.Header("diff %s vs %s/%s/%s/%s@%s",
		absDir, coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)
	out.Plain("  (hashing local files; comparing to the published asset SHAs)")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	nameWidth := pickNameColumnWidth(extractAssetNames(pub))
	termWidth := output.TerminalWidth()
	pubLabel := fmt.Sprintf("%s/%s/%s/%s@%s",
		coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)

	var matched, mismatch, missing, opErrors int
	for _, a := range pub {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		// CodeArtifact asset names are server-controlled and path-like; route
		// through the same safeJoin defense as pull so a malicious or
		// malformed name (../../etc/passwd, an absolute path, a symlinked
		// component) can't pull hashes of files outside dirPath into the
		// comparison report.
		localPath, joinErr := safeJoin(dirPath, a.Name)
		if joinErr != nil {
			opErrors++
			ar := cob.AssetResult{Name: a.Name, Source: filepath.Join(dirPath, a.Name)}
			ar.SetError(joinErr)
			renderDiffSingleFail(out, a.Name, "unsafe asset name", nameWidth,
				[]diffDetail{{"error", joinErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar := cob.AssetResult{Name: a.Name, Source: localPath}

		info, statErr := os.Stat(localPath)
		switch {
		case os.IsNotExist(statErr):
			missing++
			ar.Method = "missing-local"
			ar.Size = a.Size
			ar.SHA256 = a.SHA256
			renderDiffSingleFail(out, a.Name, "missing locally", nameWidth,
				[]diffDetail{
					{"published", fmt.Sprintf("%s   %s", rightPadSize(a.Size, 10), pubLabel)},
					{"sha256", a.SHA256},
				}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		case statErr != nil:
			opErrors++
			ar.SetError(statErr)
			renderDiffSingleFail(out, a.Name, "could not stat local file", nameWidth,
				[]diffDetail{{"error", statErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		case !info.Mode().IsRegular():
			opErrors++
			err := fmt.Errorf("not a regular file (mode %s)", info.Mode())
			ar.SetError(err)
			renderDiffSingleFail(out, a.Name, err.Error(), nameWidth, nil, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}

		localHash, hashErr := fileSHA256(localPath)
		if hashErr != nil {
			opErrors++
			ar.SetError(hashErr)
			renderDiffSingleFail(out, a.Name, "could not hash local file", nameWidth,
				[]diffDetail{{"error", hashErr.Error()}}, termWidth)
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Size = info.Size()
		ar.SHA256 = localHash

		if strings.EqualFold(localHash, a.SHA256) {
			matched++
			ar.Method = "match"
			renderDiffMatch(out, a.Name, localPath, "match", localHash,
				info.Size(), nameWidth, termWidth, verbose)
		} else {
			mismatch++
			ar.Method = "mismatch"
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: local=%s published=%s", localHash, a.SHA256)
			renderDiffMismatch(out, a.Name, nameWidth,
				diffSide{label: "local", uri: localPath, size: info.Size(), hash: localHash},
				diffSide{label: "published", uri: pubLabel, size: a.Size, hash: a.SHA256},
			)
		}
		result.Assets = append(result.Assets, ar)
	}

	out.Plain("")

	switch {
	case opErrors > 0:
		result.Status = "error"
		result.Error = fmt.Sprintf("%d asset(s) could not be checked", opErrors)
		out.Summary("FAILED: %d could not be checked, %d mismatch, %d missing locally (%d matched).",
			opErrors, mismatch, missing, matched)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	case mismatch > 0 || missing > 0:
		result.Status = "mismatch"
		result.Error = fmt.Sprintf("%d mismatch, %d missing locally", mismatch, missing)
		out.Summary("FAILED: %d mismatch, %d missing locally (%d matched).",
			mismatch, missing, matched)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	out.Summary("OK: %d files match the published version byte-for-byte.", matched)
	return out.CommandResult(result)
}

// runDiffVersions compares two published versions of the same package
// by the SHA-256 each side has recorded in CodeArtifact. The
// provenance asset is excluded (its bytes trivially differ even when
// the package didn't change). Cross-repo same-package is allowed —
// the natural "did promotion preserve the bytes?" check.
func runDiffVersions(ctx context.Context, cfg *Config, out *output.Writer, leftTarget, rightTarget string, verbose bool) error {
	left, err := manifest.ParseCoordinates(leftTarget)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "left: %s", err)
	}
	right, err := manifest.ParseCoordinates(rightTarget)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "right: %s", err)
	}
	for _, c := range []*cob.PackageCoordinates{left, right} {
		if c.Namespace == "" || c.Package == "" || c.Version == "" {
			return fail(out, "diff", cob.ExitError,
				"both arguments must be full coordinates with a version (domain/repo/ns/pkg@version)")
		}
	}
	if left.Namespace != right.Namespace || left.Package != right.Package {
		return fail(out, "diff", cob.ExitError,
			"both versions must reference the same package (%s/%s vs %s/%s)",
			left.Namespace, left.Package, right.Namespace, right.Package)
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "diff", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, left, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "left: %s", err)
	}
	if err := resolveLatestIfNeeded(ctx, right, registry, out); err != nil {
		return fail(out, "diff", codeFor(err), "right: %s", err)
	}

	cmps, err := compareVersions(ctx, registry, left, right)
	if err != nil {
		return fail(out, "diff", codeFor(err), "%s", err)
	}

	leftLabel := fmt.Sprintf("%s/%s/%s/%s@%s", left.Domain, left.Repository, left.Namespace, left.Package, left.Version)
	rightLabel := fmt.Sprintf("%s/%s/%s/%s@%s", right.Domain, right.Repository, right.Namespace, right.Package, right.Version)
	out.Header("diff %s vs %s", leftLabel, rightLabel)

	result := &cob.CommandResult{
		Command:    "diff",
		Package:    fmt.Sprintf("%s/%s: %s vs %s", left.Namespace, left.Package, left.Version, right.Version),
		Repository: fmt.Sprintf("%s/%s vs %s/%s", left.Domain, left.Repository, right.Domain, right.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	_ = verbose // version-vs-version mode shows compact +/-/~ rows; verbose has no extra detail to add yet

	var added, removed, changed, same int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, SHA256: c.RightSHA}
		switch {
		case !c.InLeft && c.InRight:
			added++
			ar.Method = "added"
			out.Plain("  + %s  (added in %s)", c.Name, right.Version)
		case c.InLeft && !c.InRight:
			removed++
			ar.Method = "removed"
			out.Plain("  - %s  (removed in %s)", c.Name, right.Version)
		case !strings.EqualFold(c.LeftSHA, c.RightSHA):
			// hex SHA-256 case-insensitive — avoid spurious drift.
			changed++
			ar.Method = "changed"
			out.Plain("  ~ %s  (%s -> %s)", c.Name, short(c.LeftSHA), short(c.RightSHA))
		default:
			same++
			ar.Method = "same"
		}
		result.Assets = append(result.Assets, ar)
	}

	drift := added + removed + changed
	out.Summary("%d added, %d removed, %d changed, %d same.", added, removed, changed, same)

	if drift > 0 {
		result.Status = "drift"
		result.Error = fmt.Sprintf("%d added, %d removed, %d changed", added, removed, changed)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	return out.CommandResult(result)
}

// versionCompare is one asset's left-vs-right comparison for the
// version-vs-version diff. SHAs come straight from CodeArtifact's
// stored asset metadata on each side, so no downloads happen.
type versionCompare struct {
	Name     string
	LeftSHA  string
	RightSHA string
	InLeft   bool
	InRight  bool
}

// compareVersions builds a union of stored asset names across two
// published versions (excluding the provenance asset, whose bytes
// trivially differ even when the package didn't change) and records
// each side's recorded SHA-256.
func compareVersions(ctx context.Context, reg *cob.Registry, left, right *cob.PackageCoordinates) ([]versionCompare, error) {
	leftAssets, err := reg.ListAssets(ctx, left)
	if err != nil {
		return nil, fmt.Errorf("listing left: %w", err)
	}
	rightAssets, err := reg.ListAssets(ctx, right)
	if err != nil {
		return nil, fmt.Errorf("listing right: %w", err)
	}

	leftByName := assetMapExcludingProvenance(leftAssets)
	rightByName := assetMapExcludingProvenance(rightAssets)

	names := make(map[string]struct{}, len(leftByName)+len(rightByName))
	for n := range leftByName {
		names[n] = struct{}{}
	}
	for n := range rightByName {
		names[n] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	out := make([]versionCompare, 0, len(sorted))
	for _, n := range sorted {
		c := versionCompare{Name: n}
		if l, ok := leftByName[n]; ok {
			c.InLeft = true
			c.LeftSHA = l.SHA256
		}
		if r, ok := rightByName[n]; ok {
			c.InRight = true
			c.RightSHA = r.SHA256
		}
		out = append(out, c)
	}
	return out, nil
}

func assetMapExcludingProvenance(assets []cob.AssetSummary) map[string]cob.AssetSummary {
	m := make(map[string]cob.AssetSummary, len(assets))
	for _, a := range assets {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		m[a.Name] = a
	}
	return m
}
