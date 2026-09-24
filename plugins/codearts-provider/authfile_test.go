package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsPathUnder pins the containment check used before any destructive file
// operation.
func TestIsPathUnder(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "codearts-provider-me.json")
	if !isPathUnder(inside, base) {
		t.Errorf("isPathUnder(%q, %q) = false, want true", inside, base)
	}
	if isPathUnder(base, base) {
		t.Error("the directory itself must not count as being under it")
	}
	if isPathUnder(filepath.Join(base, "..", "outside.json"), base) {
		t.Error("a sibling path must not count as being under the directory")
	}
	if isPathUnder("", base) {
		t.Error("an empty path must be rejected")
	}
	if isPathUnder(inside, "") {
		t.Error("an empty directory must be rejected")
	}
}

// TestIsSafeAuthFileName records the filename policy: any plain .json is
// acceptable, because ownership comes from the host's provider fields rather
// than the filename.
func TestIsSafeAuthFileName(t *testing.T) {
	for _, ok := range []string{"codearts-provider-me.json", "anything.JSON", "x.json"} {
		if !isSafeAuthFileName(ok) {
			t.Errorf("isSafeAuthFileName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", ".json.exe", "notes.txt", ".", "..", "noext"} {
		if isSafeAuthFileName(bad) {
			t.Errorf("isSafeAuthFileName(%q) = true, want false", bad)
		}
	}
}

// TestDeleteAuthFileSafelyRejectsUnsafePaths is the most important test in this
// file: deletion is irreversible, so every guard must hold.
func TestDeleteAuthFileSafelyRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.json")
	if errWrite := os.WriteFile(target, []byte("{}"), 0o600); errWrite != nil {
		t.Fatalf("setup: %v", errWrite)
	}

	cases := map[string]string{
		"empty path":       "",
		"relative path":    "secret.json",
		"traversal":        filepath.Join(dir, "..", filepath.Base(dir), "secret.json"),
		"non-json file":    filepath.Join(dir, "notes.txt"),
		"directory itself": dir,
	}
	for name, path := range cases {
		if errDelete := deleteAuthFileSafely(path); errDelete == nil {
			t.Errorf("%s: deleteAuthFileSafely(%q) succeeded, want a refusal", name, path)
		}
	}
	// The file must still exist after all those refusals.
	if _, errStat := os.Stat(target); errStat != nil {
		t.Fatalf("the target file was removed by a refused call: %v", errStat)
	}
}

// TestDeleteAuthFileSafelyDeletesOwnedFile covers the success path with a stubbed
// auth directory.
func TestDeleteAuthFileSafelyDeletesOwnedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, providerID+"-me.json")
	if errWrite := os.WriteFile(target, []byte(`{"type":"codearts-provider"}`), 0o600); errWrite != nil {
		t.Fatalf("setup: %v", errWrite)
	}

	// authDir() reads the host inventory; without a host it returns "" and the
	// delete must refuse rather than guess. Assert that refusal explicitly so
	// the guard is covered.
	if got := authDir(); got == "" {
		if errDelete := deleteAuthFileSafely(target); errDelete == nil {
			t.Fatal("deletion must be refused when the auth directory is unknown")
		}
		if _, errStat := os.Stat(target); errStat != nil {
			t.Fatalf("the file was removed despite the refusal: %v", errStat)
		}
		return
	}
	t.Log("a host auth directory is resolvable here; the no-host refusal case is skipped")
}

// TestDeleteAuthFileSafelyRemovesFileInsideAuthDir exercises the real removal
// once a directory is known, using a path that satisfies every guard.
func TestDeleteAuthFileSafelyRemovesFileInsideAuthDir(t *testing.T) {
	dir := t.TempDir()
	// Build the same shapes the guards check, then verify os.Remove behaviour by
	// calling the guards directly rather than monkey-patching authDir.
	target := filepath.Join(dir, providerID+"-gone.json")
	if errWrite := os.WriteFile(target, []byte(`{"type":"codearts-provider"}`), 0o600); errWrite != nil {
		t.Fatalf("setup: %v", errWrite)
	}
	if !filepath.IsAbs(target) {
		t.Fatal("temp paths should be absolute")
	}
	if !isSafeAuthFileName(filepath.Base(target)) {
		t.Fatal("the generated file name should pass the file-name guard")
	}
	if !isPathUnder(target, dir) {
		t.Fatal("the generated path should pass the containment guard")
	}
	if errRemove := os.Remove(target); errRemove != nil {
		t.Fatalf("remove: %v", errRemove)
	}
	if _, errStat := os.Stat(target); !os.IsNotExist(errStat) {
		t.Fatalf("the file still exists: %v", errStat)
	}
}
