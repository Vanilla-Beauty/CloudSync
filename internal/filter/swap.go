package filter

import (
	"path/filepath"
	"strings"
)

// SwapDetector identifies temporary/swap files that should not be synced
type SwapDetector struct {
	prefixes []string
	suffixes []string
	exts     map[string]bool
}

// NewSwapDetector creates a SwapDetector with common patterns
func NewSwapDetector() *SwapDetector {
	return &SwapDetector{
		prefixes: []string{
			"~$",  // Office lock files
			".#",  // Emacs lock files
			"#",   // Emacs autosave prefix
		},
		suffixes: []string{
			"~",     // Vim/Emacs backup
			".tmp",
			".swp",
			".swo",
			".temp",
			".bak",
		},
		exts: map[string]bool{
			".tmp":   true,
			".swp":   true,
			".swo":   true,
			".temp":  true,
			".bak":   true,
			".cache": true,
		},
	}
}

// IsSwapFile returns true if the filename looks like a temporary/swap file
func (sd *SwapDetector) IsSwapFile(filename string) bool {
	base := filepath.Base(filename)
	lower := strings.ToLower(base)

	for _, p := range sd.prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}

	for _, s := range sd.suffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}

	ext := strings.ToLower(filepath.Ext(base))
	if sd.exts[ext] {
		return true
	}

	return false
}
