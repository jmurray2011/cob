package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

func newVerifyCmd(cfg *Config) *cobra.Command {
	var (
		flagVersion string
		flagDeep    bool
	)

	cmd := &cobra.Command{
		Use:   "verify <manifest|coordinates|directory> [coordinates]",
		Short: "Check that bytes match published — for a manifest, a published version's own provenance, or a local directory",
		Long: "Three modes, picked from the positional argument(s):\n\n" +
			"1. Manifest (one arg, file ending .yaml/.yml): hashes each manifest " +
			"source and compares to the published asset of the same name. " +
			"Precedence: known checksum → recorded S3 origin → " +
			"cob-provenance.json → --deep download+hash.\n\n" +
			"2. Coordinates (one arg, domain/repo/ns/pkg[@version]): " +
			"self-verifies a version against its own recorded " +
			"cob-provenance.json — every recorded asset must still hash to " +
			"what provenance recorded — and prints the chain of evidence.\n\n" +
			"3. Directory + coordinates (two args, first must be a directory): " +
			"for each published asset of <coords>, looks for a file of the " +
			"same name in <dir>, hashes it, and compares to the published " +
			"SHA-256. Answers \"do these local files match what was " +
			"published?\" without any manifest in the picture.\n\n" +
			"No mutation; exits non-zero on any mismatch.",
		Example: `  # 1. Manifest mode — does the manifest still describe what was published?
  cob verify ./my-package.yaml --version 2.1.0

  # 2. Coordinates mode — audit a version against its own provenance + chain
  cob verify acme/dev/tools/my-app@2.1.0

  # 3. Directory mode — do my local files match a published version's bytes?
  cob verify ~/pulled-dir acme/dev/tools/my-app@2.1.0`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd.Context(), cfg, args, flagVersion, flagDeep)
		},
	}
	cmd.Flags().StringVar(&flagVersion, "version", "", "Package version (manifest mode: required, or set COB_VERSION; coordinates/dir modes use @version)")
	cmd.Flags().BoolVar(&flagDeep, "deep", false, "Manifest mode: download and hash sources lacking a checksum")
	return cmd
}

// runVerify dispatches to the right mode. The shape is fixed by what
// args we received:
//
//	1 arg, .yaml/.yml → manifest mode
//	1 arg, directory  → error (directory mode requires coords as arg 2)
//	1 arg, otherwise  → coordinates mode (self-verify)
//	2 args            → directory + coordinates mode (first must be a dir)
func runVerify(ctx context.Context, cfg *Config, args []string, versionFlag string, deep bool) error {
	if len(args) == 2 {
		out := newWriter(cfg)
		defer out.Close()
		// First arg must be a directory; second arg must be coordinates.
		// The verify rendering rejects the case explicitly (rather than
		// silently falling into manifest mode on a file) so a typoed
		// "verify file other-file" surfaces a real error.
		info, err := os.Stat(args[0])
		if err != nil {
			return fail(out, "verify", cob.ExitError, "first argument must be a directory (or omit it to verify a manifest/version): %s", err)
		}
		if !info.IsDir() {
			return fail(out, "verify", cob.ExitError, "first argument must be a directory when two args are given; got file %s", args[0])
		}
		return runVerifyDir(ctx, cfg, out, args[0], args[1])
	}
	target := args[0]
	if isManifestPath(target) {
		return runVerifyManifest(ctx, cfg, target, versionFlag, deep)
	}
	// Catch the obvious mistake: `cob verify /some/dir` without coords.
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		out := newWriter(cfg)
		defer out.Close()
		return fail(out, "verify", cob.ExitError,
			"%s is a directory — to verify its files against a published version, give the coordinates as a second argument:\n  cob verify %s <domain>/<repo>/<ns>/<pkg>@<version>",
			target, target)
	}
	return runVerifyCoords(ctx, cfg, target, versionFlag)
}

// runVerifyDir hashes every file in <dir> whose name matches a published
// asset of <coords> and compares to the published SHA-256. Local files
// without a matching published asset are silently left alone (they
// might be a README, source files, etc.). Published assets without a
// local file are reported as "missing locally". The cob-provenance.json
// asset is excluded — it's audit metadata, not a package file.
func runVerifyDir(ctx context.Context, cfg *Config, out *output.Writer, dirPath, coordsArg string) error {
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	coords, err := manifest.ParseCoordinates(coordsArg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "verify", cob.ExitError,
			"full coordinates required (domain/repo/namespace/package[@version]); got %q", coordsArg)
	}
	if coords.Version == "" {
		return fail(out, "verify", cob.ExitError,
			"version required: use @version or @latest in the coordinates")
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}

	pub, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}

	absDir, _ := filepath.Abs(dirPath)
	out.Header("Verifying %s against %s/%s/%s/%s@%s",
		absDir, coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version)
	out.Plain("  (hashing local files; comparing to the published asset SHAs)")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	// Column width for asset name padding — pick from the actual names
	// so the alignment looks right for this particular package.
	nameWidth := 12
	for _, a := range pub {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		if n := len(a.Name); n > nameWidth {
			nameWidth = n
		}
	}
	if nameWidth > 40 {
		nameWidth = 40 // hard cap to keep rows from getting absurd
	}

	var matched, mismatch, missing, opErrors int
	for _, a := range pub {
		if a.Name == cob.ProvenanceFile {
			continue
		}
		localPath := filepath.Join(dirPath, a.Name)
		ar := cob.AssetResult{Name: a.Name, Source: localPath}

		info, statErr := os.Stat(localPath)
		switch {
		case os.IsNotExist(statErr):
			missing++
			ar.Method = "missing-local"
			ar.Size = a.Size
			ar.SHA256 = a.SHA256
			out.Plain("  ✗ %s  missing locally (published: %s)",
				padRight(a.Name, nameWidth), output.FormatSize(a.Size))
			result.Assets = append(result.Assets, ar)
			continue
		case statErr != nil:
			opErrors++
			ar.SetError(statErr)
			out.Plain("  ✗ %s  could not stat local file: %s",
				padRight(a.Name, nameWidth), statErr)
			result.Assets = append(result.Assets, ar)
			continue
		case !info.Mode().IsRegular():
			opErrors++
			err := fmt.Errorf("not a regular file (mode %s)", info.Mode())
			ar.SetError(err)
			out.Plain("  ✗ %s  %s", padRight(a.Name, nameWidth), err)
			result.Assets = append(result.Assets, ar)
			continue
		}

		localHash, hashErr := fileSHA256(localPath)
		if hashErr != nil {
			opErrors++
			ar.SetError(hashErr)
			out.Plain("  ✗ %s  could not hash local file: %s",
				padRight(a.Name, nameWidth), hashErr)
			result.Assets = append(result.Assets, ar)
			continue
		}
		ar.Size = info.Size()
		ar.SHA256 = localHash

		if strings.EqualFold(localHash, a.SHA256) {
			matched++
			ar.Method = "match"
			// One-line match row: name • size • full sha256.
			out.Plain("  ✓ %s  %s  %s",
				padRight(a.Name, nameWidth),
				rightPadSize(info.Size(), 10),
				localHash)
		} else {
			mismatch++
			ar.Method = "mismatch"
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: local=%s published=%s", localHash, a.SHA256)
			// Multi-line mismatch row: header + both sides labeled with
			// full hashes so the operator can sha256sum the local file
			// and string-compare.
			out.Plain("  ✗ %s  mismatch", padRight(a.Name, nameWidth))
			out.Plain("      local     %s   %s   %s",
				rightPadSize(info.Size(), 10), localPath, localHash)
			out.Plain("      published %s   %s/%s/%s/%s@%s   %s",
				rightPadSize(a.Size, 10),
				coords.Domain, coords.Repository, coords.Namespace, coords.Package, coords.Version,
				a.SHA256)
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

func runVerifyManifest(ctx context.Context, cfg *Config, manifestPath, versionFlag string, deep bool) error {
	out := newWriter(cfg)
	defer out.Close()
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	version, err := resolveVersion(versionFlag)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	warnManifestOverrides(m, out)

	coords := &cob.PackageCoordinates{
		Domain: m.Domain, Repository: m.Repository,
		Namespace: m.Namespace, Package: m.Package, Version: version,
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}

	// Resolve @latest before expanding ${VERSION} — otherwise the manifest's
	// source URIs and the lookup would target a version literally "latest".
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}
	version = coords.Version

	if err := m.ResolveVariables(version); err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}

	sources, err := buildSources(m, client)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}

	prov, perr := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if perr != nil {
		// Don't silently downgrade to "unverified" — the user should know
		// the provenance fallback is unavailable.
		out.Warn("could not read %s: %s", cob.ProvenanceFile, perr)
	}
	cmps, err := compareManifestToPublished(ctx, sources, registry, coords, deep, prov)
	if err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}

	// Pub size lookup so mismatch rows can show both sides' sizes.
	pubSize := make(map[string]int64)
	if pub, perr := registry.ListAssets(ctx, coords); perr == nil {
		for _, a := range pub {
			pubSize[a.Name] = a.Size
		}
	}

	out.Header("Verifying %s/%s@%s against %s", m.Namespace, m.Package, version, manifestPath)
	out.Plain("  (hashing each manifest source; comparing to the corresponding published asset)")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", m.Namespace, m.Package, version),
		Repository: fmt.Sprintf("%s/%s", m.Domain, m.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	// Column width for asset name padding.
	nameWidth := 12
	for _, c := range cmps {
		if n := len(c.Name); n > nameWidth {
			nameWidth = n
		}
	}
	if nameWidth > 40 {
		nameWidth = 40
	}

	var failures, opErrors, unverified, extra int
	for _, c := range cmps {
		ar := cob.AssetResult{Name: c.Name, Source: c.Source, SHA256: c.SrcSHA}
		switch {
		case !c.InManifest && c.InPublished:
			extra++
			ar.Method = "extra"
			out.Plain("  ⊘ %s  (published, not in manifest)", padRight(c.Name, nameWidth))
		case c.Err != nil:
			// An operational error (couldn't hash a source, network) — the
			// check did not complete; distinct from a real mismatch.
			opErrors++
			ar.SetError(c.Err)
			out.Plain("  ✗ %s  could not check: %s", padRight(c.Name, nameWidth), c.Err)
			out.Plain("      source     %s", c.Source)
		case !c.InPublished:
			failures++
			ar.Method = "missing"
			out.Plain("  ✗ %s  not published in @%s", padRight(c.Name, nameWidth), version)
			out.Plain("      source     %s   %s", c.Source, c.SrcSHA)
		case c.OriginDrift:
			failures++
			ar.Method = "drift"
			out.Plain("  ✗ %s  S3 source changed since publish (etag/version differs from provenance)",
				padRight(c.Name, nameWidth))
			out.Plain("      source     %s", c.Source)
		case c.SrcSHA == "":
			unverified++
			ar.Method = "unverified"
			out.Plain("  ⊘ %s  unverified — no checksum/provenance (use --deep to hash)",
				padRight(c.Name, nameWidth))
			out.Plain("      source     %s", c.Source)
		case strings.EqualFold(c.SrcSHA, c.PubSHA):
			// hex SHA-256 is case-insensitive — different sources/SDKs
			// return upper- vs lower-hex; EqualFold avoids a spurious
			// mismatch.
			ar.Method = "match(" + c.SrcFrom + ")"
			out.Plain("  ✓ %s  %s   match(%s)",
				padRight(c.Name, nameWidth), c.Source, c.SrcFrom)
			out.Plain("      sha256     %s", c.SrcSHA)
		default:
			failures++
			ar.Method = "mismatch"
			ar.ErrorMsg = fmt.Sprintf("SHA-256 mismatch: source=%s published=%s", c.SrcSHA, c.PubSHA)
			out.Plain("  ✗ %s  mismatch", padRight(c.Name, nameWidth))
			out.Plain("      source     %s   %s   %s",
				rightPadSize(0, 10), c.Source, c.SrcSHA)
			out.Plain("      published  %s   %s/%s/%s/%s@%s   %s",
				rightPadSize(pubSize[c.Name], 10),
				m.Domain, m.Repository, m.Namespace, m.Package, version,
				c.PubSHA)
		}
		result.Assets = append(result.Assets, ar)
	}

	out.Plain("")

	if extra > 0 {
		out.Warn("%d published asset(s) not declared in the manifest", extra)
	}
	if unverified > 0 {
		out.Warn("%d source(s) unverified (no checksum or provenance; pass --deep to hash)", unverified)
	}

	// An operational error means the check did not complete — exit "error".
	// A clean run that found mismatches/drift exits "mismatch" (code 4), so a
	// CI gate can tell "the bytes changed" from "the check failed to run".
	if opErrors > 0 {
		result.Status = "error"
		result.Error = fmt.Sprintf("%d asset(s) could not be checked", opErrors)
		out.Summary("FAILED: %d asset(s) could not be checked.", opErrors)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitError}
	}
	if failures > 0 {
		result.Status = "mismatch"
		result.Error = fmt.Sprintf("%d asset(s) do not match the manifest", failures)
		out.Summary("FAILED: %d mismatched/missing.", failures)
		out.CommandResult(result)
		return &ExitError{Code: cob.ExitMismatch}
	}
	out.Summary("OK: published version matches the manifest.")
	return out.CommandResult(result)
}

// runVerifyCoords self-verifies a published version against its own recorded
// cob-provenance.json — no manifest, no source access. Every recorded asset
// must still hash to what provenance recorded, and the chain of evidence is
// printed.
func runVerifyCoords(ctx context.Context, cfg *Config, target, versionFlag string) error {
	out := newWriter(cfg)
	defer out.Close()
	ctx, cancel := interruptable(ctx, out)
	defer cancel()

	coords, err := manifest.ParseCoordinates(target)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	if coords.Namespace == "" || coords.Package == "" {
		return fail(out, "verify", cob.ExitError, "full coordinates required (domain/repo/namespace/package[@version])")
	}
	if coords.Version == "" {
		v, verr := resolveVersion(versionFlag)
		if verr != nil {
			return fail(out, "verify", cob.ExitError, "%s", verr)
		}
		coords.Version = v
	}

	client, err := dialClient(ctx, cfg)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	registry := cob.NewRegistry(client)
	if err := resolveLatestIfNeeded(ctx, coords, registry, out); err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}

	prov, err := cob.FetchProvenance(ctx, client.CodeArtifact, coords)
	if err != nil {
		return fail(out, "verify", cob.ExitError, "%s", err)
	}
	if prov == nil {
		return fail(out, "verify", cob.ExitError,
			"no %s for %s/%s@%s — cannot self-verify (was it published with cob?)",
			cob.ProvenanceFile, coords.Namespace, coords.Package, coords.Version)
	}

	assets, err := registry.ListAssets(ctx, coords)
	if err != nil {
		return fail(out, "verify", codeFor(err), "%s", err)
	}
	pubSHA := make(map[string]string, len(assets))
	pubSize := make(map[string]int64, len(assets))
	for _, a := range assets {
		pubSHA[a.Name] = a.SHA256
		pubSize[a.Name] = a.Size
	}

	out.Header("Self-verifying %s/%s@%s in %s/%s against recorded provenance",
		coords.Namespace, coords.Package, coords.Version, coords.Domain, coords.Repository)
	renderChain(out, prov, nil)
	renderOrigins(out, prov, "")
	out.Plain("")

	result := &cob.CommandResult{
		Command:    "verify",
		Package:    fmt.Sprintf("%s/%s@%s", coords.Namespace, coords.Package, coords.Version),
		Repository: fmt.Sprintf("%s/%s", coords.Domain, coords.Repository),
		Status:     "ok",
	}
	fillClientMeta(ctx, client, result)

	nameWidth := 12
	for _, e := range prov.Assets {
		if n := len(e.Asset); n > nameWidth {
			nameWidth = n
		}
	}
	if nameWidth > 40 {
		nameWidth = 40
	}

	var failures int
	for _, e := range prov.Assets {
		ar := cob.AssetResult{Name: e.Asset, Source: e.Source, SHA256: e.SHA256, Size: e.Size}
		cur, ok := pubSHA[e.Asset]
		switch {
		case !ok:
			failures++
			ar.Method = "missing"
			out.Plain("  ✗ %s  recorded in provenance but not in the published version",
				padRight(e.Asset, nameWidth))
			out.Plain("      recorded   %s   %s", rightPadSize(e.Size, 10), e.SHA256)
		case strings.EqualFold(cur, e.SHA256):
			// hex SHA-256 case-insensitive — see compareManifestToPublished.
			ar.Method = "match(provenance)"
			out.Plain("  ✓ %s  %s   match(provenance)",
				padRight(e.Asset, nameWidth), rightPadSize(e.Size, 10))
			out.Plain("      sha256     %s", e.SHA256)
		default:
			failures++
			ar.Method = "altered"
			ar.ErrorMsg = fmt.Sprintf("altered since publish: recorded=%s published=%s", e.SHA256, cur)
			out.Plain("  ✗ %s  altered since publish", padRight(e.Asset, nameWidth))
			out.Plain("      recorded   %s   %s", rightPadSize(e.Size, 10), e.SHA256)
			out.Plain("      published  %s   %s", rightPadSize(pubSize[e.Asset], 10), cur)
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

// fileSHA256 streams a file through sha256 and returns the lowercase
// hex digest. Used by directory mode to hash local files in constant
// memory, regardless of how large any single file is.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// padRight pads s with spaces to width characters; if s is already
// wider, returns it unmodified (we'd rather a row wrap than truncate
// the asset name a user is trying to read).
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// rightPadSize formats a byte count and right-pads it to width so a
// column of sizes aligns on the unit (KB/MB/GB) rather than the digits.
func rightPadSize(n int64, width int) string {
	s := output.FormatSize(n)
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
