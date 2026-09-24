package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Deleting an auth file is destructive and the plugin ABI offers no
// host-mediated delete, so the operation is confined to a plain .json file
// inside the host's auth directory. These guards exist to make a buggy or
// malicious path from the host unable to delete something unrelated.

// authDir resolves the host's auth directory from the host config summary.
func authDir() string {
	// host.auth.list entries carry the absolute path; the auth directory is
	// derived from the first entry that has one, which avoids needing a separate
	// config callback.
	files, errList := hostAuthList()
	if errList != nil {
		return ""
	}
	for _, file := range files {
		if path := strings.TrimSpace(file.Path); path != "" {
			return filepath.Dir(path)
		}
	}
	return ""
}

// isSafeAuthFileName reports whether base is a plain JSON credential file.
//
// Provider ownership is established separately, from the host's provider/type
// fields, so this deliberately does not require a filename prefix: an operator
// may legitimately have dropped a credential file in under any name.
func isSafeAuthFileName(base string) bool {
	base = strings.TrimSpace(base)
	if base == "" {
		return false
	}
	if base == "." || base == ".." {
		return false
	}
	return strings.HasSuffix(strings.ToLower(base), ".json")
}

// isPathUnder reports whether path is strictly inside dir.
func isPathUnder(path, dir string) bool {
	path = strings.TrimSpace(path)
	dir = strings.TrimSpace(dir)
	if path == "" {
		return false
	}
	if dir == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanDir := filepath.Clean(dir)
	if cleanPath == cleanDir {
		return false
	}
	rel, errRel := filepath.Rel(cleanDir, cleanPath)
	if errRel != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)+"..")
}

// deleteAuthFileSafely removes one credential file after validating that it is
// a plain, absolute .json file belonging to this plugin and living directly in
// the auth directory.
func deleteAuthFileSafely(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("credential file path is missing")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("refusing to delete a relative path: %s", path)
	}
	slashed := filepath.ToSlash(path)
	if strings.Contains(slashed, "../") || strings.Contains(slashed, "/..") {
		return fmt.Errorf("refusing to delete a path containing traversal segments: %s", path)
	}
	base := filepath.Base(path)
	if base != filepath.Base(filepath.Clean(path)) {
		return fmt.Errorf("refusing to delete an ambiguous path: %s", path)
	}
	if !isSafeAuthFileName(base) {
		return fmt.Errorf("refusing to delete %q: it is not a %s credential file", base, providerID)
	}
	dir := authDir()
	if dir == "" {
		return fmt.Errorf("the host auth directory could not be determined; refusing to delete %s", path)
	}
	if !isPathUnder(path, dir) {
		return fmt.Errorf("refusing to delete %s: it is outside the auth directory", path)
	}
	if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
		return fmt.Errorf("delete %s: %w", filepath.Base(path), errRemove)
	}
	return nil
}
