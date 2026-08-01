package metaops

import (
	"fmt"
	"path/filepath"
	"strings"
)

// PathAllowed reports whether target is permitted as a scan root given the
// configured library_roots allow-list. An empty roots list allows any local path
// (the loopback trust model). When roots is non-empty, target must resolve to a
// path inside one of them. Both target and each root are resolved to absolute,
// symlink-evaluated paths first, so a symlink cannot smuggle a scan outside an
// allowed root. It is security-relevant, so it is a pure, separately-tested
// function rather than inline handler logic.
func PathAllowed(target string, roots []string) (bool, error) {
	if len(roots) == 0 {
		return true, nil
	}
	rt, err := resolvePath(target)
	if err != nil {
		return false, err
	}
	return containedIn(rt, roots), nil
}

// pathAllowedResolved is PathAllowed's containment half, for a target already
// resolved. Keeping it separate is what lets AllowedPath resolve exactly once.
func pathAllowedResolved(resolved string, roots []string) (bool, error) {
	if len(roots) == 0 {
		return true, nil
	}
	return containedIn(resolved, roots), nil
}

// containedIn reports whether the already-resolved rt sits inside any of roots.
func containedIn(rt string, roots []string) bool {
	for _, root := range roots {
		rr, err := resolvePath(root)
		if err != nil {
			continue // an unresolvable configured root simply cannot match
		}
		if rt == rr || strings.HasPrefix(rt, rr+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// AllowedPath is PathAllowed plus the canonical path it checked.
//
// Callers that PERSIST a path must store this rather than the raw string they were
// given: keeping the raw form leaves a window in which a symlink that passed the
// allow-list check is repointed before the path is used, so the path checked and the
// path opened are not the same one. It also resolves once instead of twice, which
// matters in the per-candidate loop that enqueues a whole library over SMB.
//
// ok is false with a nil error when the path simply falls outside the allow-list.
func AllowedPath(target string, roots []string) (resolved string, ok bool, err error) {
	resolved, err = resolvePath(target)
	if err != nil {
		return "", false, err
	}
	ok, err = pathAllowedResolved(resolved, roots)
	return resolved, ok, err
}

// resolvePath returns the absolute, symlink-evaluated form of p. It falls back to
// the cleaned absolute path when the target does not yet exist (EvalSymlinks
// needs an existing path), which is safe because a non-existent path cannot be
// scanned anyway.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		return eval, nil
	}
	return filepath.Clean(abs), nil
}
