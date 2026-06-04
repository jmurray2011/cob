package cliutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmurray2011/cob/internal/cob"
	"github.com/jmurray2011/cob/internal/manifest"
	"github.com/jmurray2011/cob/internal/output"
)

// Interruptable wraps ctx with cancellation hooks for every long-running
// operation: a TTY Ctrl-C rescue (bubbletea's raw mode otherwise swallows
// the signal — main's signal.NotifyContext can't see it while the TUI is
// up) and, when cfg.Timeout > 0, a deadline. The returned cancel fires
// both, in reverse order. Caller defers it.
func Interruptable(ctx context.Context, cfg *Config, out *output.Writer) (context.Context, context.CancelFunc) {
	var cancels []context.CancelFunc
	if cfg != nil && cfg.Timeout > 0 {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, cfg.Timeout)
		cancels = append(cancels, tcancel)
	}
	ctx, icancel := context.WithCancel(ctx)
	cancels = append(cancels, icancel)
	out.SetInterrupt(icancel)
	return ctx, func() {
		// Fire in reverse: cancel the user-rescue first so the SetInterrupt
		// hook is the most-recent observer to see "we're done", then collapse
		// the timeout's timer goroutine.
		for i := len(cancels) - 1; i >= 0; i-- {
			cancels[i]()
		}
	}
}

// FillClientMeta records the executing principal and region onto a
// CommandResult so audit pipelines can answer "which account/role ran
// this, in which region?" without grepping per-asset Origin or the
// provenance chain. Called right before out.CommandResult(result) on
// every command that has a live cob.Client; safe to call multiple times
// (CallerIdentity is cached on the Client).
func FillClientMeta(ctx context.Context, client *cob.Client, result *cob.CommandResult) {
	if client == nil || result == nil {
		return
	}
	result.Region = client.Region
	a := client.CallerIdentity(ctx)
	if a.ARN != "" || a.UserID != "" || a.Account != "" {
		result.Actor = &a
	}
}

// ResolveVersion returns the version from the flag, env var, or an error.
func ResolveVersion(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("COB_VERSION"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("version is required: use --version or set COB_VERSION")
}

// IsManifestPath returns true if the arg looks like a manifest file path.
// Suffix-only detection — a coordinate-shaped string that happens to end
// in .yaml (e.g. a package literally named "config.yaml") would be
// misclassified, but that ambiguity is rare enough in practice that we
// surface it via the routed-error path rather than adding a stat probe.
// filepath.Ext over HasSuffix gets us the right behavior on weird inputs
// (a trailing dot, ".YAML" in case-sensitive land, dotfiles like ".yaml"
// at the path root) without growing the rule.
func IsManifestPath(arg string) bool {
	switch filepath.Ext(arg) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// WarnManifestOverrides logs any env var overrides applied to the manifest.
func WarnManifestOverrides(m *manifest.Manifest, out *output.Writer) {
	for _, o := range m.Overrides {
		out.Warn("using %s=%s (overrides manifest %s)", o.Env, o.Value, o.Field)
	}
}

// FirstResultError returns the message of the first failed asset result —
// for the partial-failure summary pull/publish/promote each print. nil
// results are skipped; the generic fallback covers a batch that failed
// without any per-asset message (e.g. an early cancellation).
func FirstResultError(results []*cob.AssetResult) string {
	for _, r := range results {
		if r != nil && r.ErrorMsg != "" {
			return r.ErrorMsg
		}
	}
	return "asset transfer failed"
}

// ResolveLatestIfNeeded checks if coords.Version is "latest" and, if so,
// resolves it to the most recently published version. Prints the resolved
// version so the user knows what they got.
func ResolveLatestIfNeeded(ctx context.Context, coords *cob.PackageCoordinates, registry *cob.Registry, out *output.Writer) error {
	if coords.Version != "latest" {
		return nil
	}
	version, err := registry.ResolveLatest(ctx, coords)
	if err != nil {
		return err
	}
	coords.Version = version
	out.Header("Resolved latest -> %s", version)
	return nil
}

// ConfirmAction decides whether a mutating action may proceed.
//
//   - --yes: always proceeds.
//   - interactive TTY: prompts; only a "y"/"yes" answer proceeds.
//   - non-interactive (piped / CI) without --yes: refuses with an error
//     instead of silently auto-confirming. Auto-confirming a delete or
//     publish in a pipeline is a footgun; callers must pass --yes to opt in.
//
// A non-nil error means the action must not run and the command should fail
// loudly. (false, nil) means the user declined at the prompt — a clean,
// expected abort.
func ConfirmAction(ctx context.Context, yes bool, prompt string) (bool, error) {
	if yes {
		return true, nil
	}
	if !output.IsTerminal(os.Stdin) {
		return false, fmt.Errorf("refusing to proceed without confirmation: stdin is not a TTY; pass --yes to confirm")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)

	// Read in a goroutine so a SIGINT (which cancels ctx) interrupts the
	// prompt — fmt.Scanln is a blocking stdin read with no context awareness,
	// so without this Ctrl-C does nothing until the user presses Enter. The
	// buffered channel lets the goroutine finish even if ctx won the select.
	ch := make(chan string, 1)
	go func() {
		var response string
		_, _ = fmt.Scanln(&response) // a read error declines; nothing to handle
		ch <- response
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr) // move off the "[y/N] " line
		return false, ctx.Err()
	case response := <-ch:
		return strings.HasPrefix(strings.ToLower(response), "y"), nil
	}
}

// FinalizeProvenance publishes the cob-provenance.json finalizer for a
// freshly published or promoted version and folds the result into cmdResult.
// Publishing it with unfinished=false also flips the CodeArtifact version to
// Published. On failure it emits failureHint — the full command-specific
// operator guidance, since publish and promote recover differently — and
// returns a non-zero ExitError for the caller to return as-is.
func FinalizeProvenance(ctx context.Context, publisher *cob.Publisher, coords *cob.PackageCoordinates,
	prov *cob.Provenance, out *output.Writer, cmdResult *cob.CommandResult, start time.Time, failureHint string) error {

	provBytes, err := prov.Marshal()
	if err != nil {
		// Marshal only fails today on size — the depth cap caught most of
		// it, but a fan-out-heavy upstream can still tip over. Drop every
		// inlined upstream and retry; the chain coords on each Origin
		// survive, so the trail isn't lost, just no longer self-contained.
		out.Warn("provenance too large to embed upstream chains (%v); retrying with them dropped", err)
		cob.PruneUpstreamProvenance(prov, 0)
		provBytes, err = prov.Marshal()
		if err != nil {
			cmdResult.DurationMs = time.Since(start).Milliseconds()
			cmdResult.Status = "error"
			cmdResult.Error = err.Error()
			out.Error("%s\n  %s", err, failureHint)
			out.CommandResult(cmdResult)
			return &ExitError{Code: cob.ExitError}
		}
	}
	provSrc := cob.NewBytesSource(cob.ProvenanceFile, provBytes)
	out.AssetStart(cob.ProvenanceFile, "", 0)
	par, err := publisher.PublishAsset(ctx, coords, cob.ProvenanceFile, provSrc, false)
	if err != nil {
		out.AssetFail(cob.ProvenanceFile, "", err)
		cmdResult.DurationMs = time.Since(start).Milliseconds()
		cmdResult.Status = "error"
		cmdResult.Error = err.Error()
		out.Error("%s\n  %s", err, failureHint)
		out.CommandResult(cmdResult)
		return &ExitError{Code: cob.ExitError}
	}
	out.AssetOK(par, "")
	cmdResult.Assets = append(cmdResult.Assets, *par)
	cmdResult.TotalSize += par.Size
	cmdResult.DurationMs = time.Since(start).Milliseconds()
	return nil
}
