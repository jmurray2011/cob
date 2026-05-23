package cliutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jmurray2011/cob/internal/output"
)

// currentPackageFile is the on-disk name for the "current package" coord
// pointer. `.cob/current` is project-local; `~/.config/cob/current` is
// the global fallback. The file is one line: the raw coordinate string
// (no trailing newline required). Anything else is treated as malformed
// and reported with a clear error so a corrupt file doesn't silently
// reroute a command.
const currentPackageFile = "current"
const currentPackageDir = ".cob"

// CurrentPackageSource names where a resolved current package came from,
// so commands can surface "Using <coords> (from <source>)" to the
// operator. A user who forgot they ran `cob use` last week shouldn't be
// surprised by which package a no-arg command picked up.
//
// For flag/env sources the value is a fixed label; for disk sources it
// is the path of the file that won — prettified so a cwd-local hit reads
// "./.cob/current" and a home hit reads "~/.config/cob/current", but a
// walk-up hit from a deep cwd shows the **absolute path of the ancestor**
// so the operator can tell "this came from my project root .cob/current"
// from "this came from /home/me/.cob/current set last month and
// forgotten about." A "from .cob/current" message that's ambiguous
// between those two is the exact footgun this naming avoids.
type CurrentPackageSource string

const (
	SourceFlag    CurrentPackageSource = "--package flag"
	SourceArg     CurrentPackageSource = "positional"
	SourceEnv     CurrentPackageSource = "COB_PACKAGE_COORDS"
	SourceUnknown CurrentPackageSource = ""
)

// CurrentPackage returns the package coordinates to use when no
// positional was given, along with where it came from. Precedence:
//
//  1. --package flag on the root command (cfg.PackageOverride)
//  2. COB_PACKAGE_COORDS env (already merged into cfg by
//     ApplyEnvFallbacks; this function reads cfg.PackageOverride
//     either way)
//  3. .cob/current in the cwd, walking up parent dirs to filesystem root
//  4. ~/.config/cob/current (global)
//
// Returns ("", SourceUnknown, nil) when nothing is set — the caller
// reports "no coordinates and no current package; pass <coords> or run
// `cob use <coords>` first." Returns a non-nil error only on a corrupt
// state file; an unreadable home config is treated as absent so a stale
// permission on ~/.config never blocks a command that doesn't need it.
func CurrentPackage(cfg *Config) (coords string, src CurrentPackageSource, err error) {
	if cfg != nil && cfg.PackageOverride != "" {
		// --package flag takes precedence over env in ApplyEnvFallbacks,
		// but the Source string here is whatever populated cfg first.
		// In practice the flag's Changed() guard in ApplyEnvFallbacks
		// keeps the two from racing.
		return cfg.PackageOverride, cfg.PackageOverrideSource, nil
	}
	if v, path, ok := readCwdCurrent(); ok {
		return v, prettifyPath(path), nil
	}
	if v, path, ok := readHomeCurrent(); ok {
		return v, prettifyPath(path), nil
	}
	return "", SourceUnknown, nil
}

// prettifyPath turns an absolute path into the source label rendered
// after "Using X (from ...)". The format follows operator intuition:
//
//   - Cwd-local hit (./.cob/current of the current dir): "./.cob/current"
//     — short and unambiguous because the user is standing in this dir.
//   - Home hit: "~/.config/cob/current" — recognizable across machines.
//   - Anywhere else (walk-up from a deep cwd, custom XDG_CONFIG_HOME, …):
//     the absolute path, so a walk-up to /home/me/work/.cob/current
//     reads "from /home/me/work/.cob/current" instead of the ambiguous
//     "from .cob/current" (which directory's?).
func prettifyPath(absPath string) CurrentPackageSource {
	abs, err := filepath.Abs(absPath)
	if err != nil {
		return CurrentPackageSource(absPath)
	}
	if cwd, err := os.Getwd(); err == nil {
		if cwdAbs, err := filepath.Abs(cwd); err == nil {
			cwdLocal := filepath.Join(cwdAbs, currentPackageDir, currentPackageFile)
			if filepath.Clean(abs) == filepath.Clean(cwdLocal) {
				return CurrentPackageSource("./" + filepath.Join(currentPackageDir, currentPackageFile))
			}
		}
	}
	if home, err := homeCobDir(); err == nil {
		homeFile := filepath.Join(home, currentPackageFile)
		if filepath.Clean(abs) == filepath.Clean(homeFile) {
			// Render under ~ when the path is inside the user's home —
			// works whether home is XDG-relocated or the OS default.
			if userHome, err := os.UserHomeDir(); err == nil {
				if rel, err := filepath.Rel(userHome, abs); err == nil && !strings.HasPrefix(rel, "..") {
					return CurrentPackageSource("~/" + filepath.ToSlash(rel))
				}
			}
		}
	}
	return CurrentPackageSource(abs)
}

// SetCurrentPackage writes coords as the current package. When global is
// true, writes to ~/.config/cob/current; otherwise to ./.cob/current
// (creating the dir if missing). Returns the absolute path written so
// the caller can echo it back.
func SetCurrentPackage(coords string, global bool) (path string, err error) {
	coords = strings.TrimSpace(coords)
	if coords == "" {
		return "", errors.New("refusing to set an empty current package")
	}
	var dir string
	if global {
		dir, err = homeCobDir()
		if err != nil {
			return "", err
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cwd, currentPackageDir)
	}
	// 0o700 — matches the TmpDir convention; the file holds operational
	// intent ("the package I'm working on"), not a secret, but on a
	// shared host that's still per-user metadata.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path = filepath.Join(dir, currentPackageFile)
	if err := os.WriteFile(path, []byte(coords+"\n"), 0o600); err != nil {
		return "", err
	}
	// Drop a .gitignore so a project-local .cob/ doesn't leak into git
	// by default. Idempotent: only write if the file doesn't already
	// exist (so we don't trample an operator who added something custom).
	if !global {
		giPath := filepath.Join(dir, ".gitignore")
		if _, err := os.Stat(giPath); os.IsNotExist(err) {
			_ = os.WriteFile(giPath, []byte("# Local cob state. Per-developer; don't commit.\ncurrent\n"), 0o600)
		}
	}
	return path, nil
}

// ClearCurrentPackage removes the current-package file. global controls
// whether the cwd or the home file is targeted, matching SetCurrentPackage.
// Missing file is not an error — `cob use --clear` should be idempotent.
func ClearCurrentPackage(global bool) (path string, err error) {
	if global {
		dir, err := homeCobDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, currentPackageFile)
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = filepath.Join(cwd, currentPackageDir, currentPackageFile)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return path, err
	}
	return path, nil
}

// readCwdCurrent walks from cwd up to the filesystem root looking for a
// .cob/current. Returns (coords, absolute-path-found, true) on the first
// hit. The path is what gets prettified into the source attribution so
// a walk-up from a deep cwd surfaces the actual ancestor location, not
// a generic "parent .cob/current" that hides which ancestor.
func readCwdCurrent() (string, string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", false
	}
	dir, _ := findCobDir(cwd)
	if dir == "" {
		return "", "", false
	}
	path := filepath.Join(dir, currentPackageFile)
	v, ok := readCoordsFile(path)
	return v, path, ok
}

// findCobDir walks up from start looking for a .cob directory and
// returns its absolute path. Returns "" when nothing's found before the
// filesystem root.
func findCobDir(start string) (string, error) {
	dir := start
	for {
		candidate := filepath.Join(dir, currentPackageDir)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// readHomeCurrent reads ~/.config/cob/current. Missing or unreadable
// home config is treated as "no global current" rather than an error,
// so a broken permission doesn't block commands that wouldn't need it.
// Returns the absolute path alongside the coords for source attribution.
func readHomeCurrent() (string, string, bool) {
	dir, err := homeCobDir()
	if err != nil {
		return "", "", false
	}
	path := filepath.Join(dir, currentPackageFile)
	v, ok := readCoordsFile(path)
	return v, path, ok
}

// homeCobDir resolves ~/.config/cob (honoring XDG_CONFIG_HOME).
func homeCobDir() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "cob"), nil
}

// readCoordsFile loads a one-line coordinate file. Returns (coords, true)
// on success; (_, false) on any read error. The string is trimmed and
// must be non-empty after trim — an empty file is treated as "no current
// package," not as a corrupt one, so an operator who accidentally
// emptied the file gets the no-current behavior instead of a hard error.
func readCoordsFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", false
	}
	// Reject obviously-malformed content (multi-line, embedded nulls).
	// A well-formed coords string fits on one line.
	if strings.ContainsAny(v, "\n\x00") {
		return "", false
	}
	return v, true
}

// PackageOverrideMissingErr is the error every read-only command
// returns when no positional arg was given AND no current package is
// set. Phrased to point at the fix.
func PackageOverrideMissingErr(cmd string) error {
	return fmt.Errorf(
		"no coordinates and no current package set — pass <coordinates> or run `cob use <coordinates>` first",
	)
}

// ResolveTarget returns the coordinate target for a read-only command.
// If positional was given, that wins. Otherwise it falls back to the
// current package (--package > COB_PACKAGE_COORDS > .cob/current >
// ~/.config/cob/current). When the fallback fires, an "Using <coords>
// (from <source>)" header is emitted via out so the operator sees which
// source rerouted them — silent inheritance is exactly the spooky-action
// behavior the source attribution avoids.
//
// command is the bare command name (e.g. "log", "diff") used only for
// the error message when no current package is set.
//
// Destructive commands (rm, publish, promote) deliberately do NOT call
// this — they pull args[0] directly and surface the "missing positional"
// error from cobra, on the grounds that an operator who cd's into the
// wrong project shouldn't have rm silently target last week's `cob use`
// setting.
func ResolveTarget(cfg *Config, out *output.Writer, args []string, command string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	coords, src, err := CurrentPackage(cfg)
	if err != nil {
		return "", err
	}
	if coords == "" {
		return "", PackageOverrideMissingErr(command)
	}
	// Notice → stderr, no prefix. Visible to an interactive operator,
	// invisible to a script that captured stdout (`$(cob resolve)`).
	out.Notice("Using %s (from %s)", coords, src)
	return coords, nil
}
