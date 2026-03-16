package utils

import (
	"os"
	"path/filepath"
	"strings"
)

// FileExists returns true if path exists and is not a directory
func FileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// DirExists returns true if path exists and is a directory
func DirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// ToSlashPath converts OS-specific path separators to forward slashes
func ToSlashPath(path string) string {
	return filepath.ToSlash(path)
}

// RelPath returns the relative path of target from base, using forward slashes
func RelPath(base, target string) (string, error) {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// NormalizeRemotePrefix converts a user-supplied remote prefix to a canonical
// forward-slash form ending with exactly one "/".
// It converts OS path separators (e.g. Windows backslashes) and strips any
// trailing slashes before appending the final "/".
//
//	"working"   → "working/"
//	"working/"  → "working/"
//	"working\"  → "working/"    (Windows input)
//	"a/b/c/"   → "a/b/c/"
func NormalizeRemotePrefix(prefix string) string {
	return strings.TrimRight(filepath.ToSlash(prefix), "/") + "/"
}

// WalkDirs calls fn for every subdirectory under root (including root itself)
func WalkDirs(root string, fn func(dir string) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable paths
		}
		if info.IsDir() {
			return fn(path)
		}
		return nil
	})
}
