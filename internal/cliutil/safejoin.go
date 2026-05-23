package cliutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SafeJoin joins an asset name under root and confirms the result stays
// within root — lexically (no ../ traversal) and physically (no symlink at
// root or at an existing component of the destination redirects the write).
// CodeArtifact asset names are path-like and server-controlled; cob must not
// write outside the chosen output directory regardless of what they contain.
//
// Used by both pull (where writes go) and diff dir mode (where reads happen)
// so a server-supplied name like "../../etc/passwd" can't escape the
// user-chosen scope in either direction.
func SafeJoin(root, name string) (string, error) {
	dest := filepath.Join(root, name)
	if escapes(root, dest) {
		return "", fmt.Errorf("asset %q escapes the output directory", name)
	}
	// Lexical containment is not enough: a symlink at root, or at any
	// existing component of dest, could redirect the write elsewhere.
	// Resolve symlinks on the deepest existing prefix of each and re-check.
	realRoot, err := resolveExisting(root)
	if err != nil {
		return "", err
	}
	realDest, err := resolveExisting(dest)
	if err != nil {
		return "", err
	}
	if escapes(realRoot, realDest) {
		return "", fmt.Errorf("asset %q escapes the output directory via a symlink", name)
	}
	return dest, nil
}

// escapes reports whether dest lies outside root by lexical path comparison.
func escapes(root, dest string) bool {
	rel, err := filepath.Rel(root, dest)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// resolveExisting returns filepath.EvalSymlinks of the deepest ancestor of p
// that exists. A pull target usually does not exist yet, so EvalSymlinks(p)
// itself would fail; resolving the existing prefix is what containment needs.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p, nil // reached the filesystem root; nothing existed
		}
		p = parent
	}
}
