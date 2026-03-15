package filter

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// IgnoreRules holds compiled patterns for file filtering
type IgnoreRules struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	re     *regexp.Regexp
	negate bool
}

// LoadIgnoreRules parses a .syncignore style file
func LoadIgnoreRules(path string) (*IgnoreRules, error) {
	ir := &IgnoreRules{}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ir, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := ir.AddPattern(line); err != nil {
			// Skip invalid patterns
			continue
		}
	}
	return ir, scanner.Err()
}

// AddPattern compiles and appends a gitignore-style glob pattern
func (ir *IgnoreRules) AddPattern(pattern string) error {
	negate := false
	if strings.HasPrefix(pattern, "!") {
		negate = true
		pattern = pattern[1:]
	}

	re, err := globToRegexp(pattern)
	if err != nil {
		return err
	}
	ir.patterns = append(ir.patterns, ignorePattern{re: re, negate: negate})
	return nil
}

// Match returns true if the path should be ignored
func (ir *IgnoreRules) Match(filePath string) bool {
	// Normalize to forward slashes
	cleaned := filepath.ToSlash(filePath)
	base := filepath.Base(filePath)

	ignored := false
	for _, p := range ir.patterns {
		// Match against both full path and basename
		if p.re.MatchString(cleaned) || p.re.MatchString(base) {
			if p.negate {
				ignored = false
			} else {
				ignored = true
			}
		}
	}
	return ignored
}

// globToRegexp converts a gitignore-style glob to a regexp
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	// Directory pattern: ends with /
	dirOnly := strings.HasSuffix(pattern, "/")
	if dirOnly {
		pattern = strings.TrimSuffix(pattern, "/")
	}

	// Escape regexp metacharacters except * and ?
	var sb strings.Builder
	sb.WriteString("(?i)") // case-insensitive on Windows is common; keep it for cross-platform

	// Anchored if pattern starts with /
	if strings.HasPrefix(pattern, "/") {
		sb.WriteString("^")
		pattern = pattern[1:]
	}

	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// ** matches any path segment
				sb.WriteString(".*")
				i += 2
				// skip optional trailing /
				if i < len(pattern) && pattern[i] == '/' {
					i++
				}
			} else {
				// * matches anything except /
				sb.WriteString("[^/]*")
				i++
			}
		case '?':
			sb.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '{', '}', '[', ']', '^', '$', '|', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
			i++
		default:
			sb.WriteByte(c)
			i++
		}
	}

	if dirOnly {
		sb.WriteString("(/.*)?$")
	} else {
		sb.WriteString("$")
	}

	return regexp.Compile(sb.String())
}
