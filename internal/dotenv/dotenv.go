// Package dotenv resolves configuration values from the process environment
// with a fallback to simple KEY=VALUE .env files. It exists so the cmd/
// binaries share one implementation instead of each carrying a copy.
//
// Values are never logged by this package; callers must take the same care.
package dotenv

import (
	"bufio"
	"os"
	"strings"
)

// Get returns key from the process environment, or from the first file in
// files that defines it, or "" if not found anywhere. Files that do not
// exist or cannot be read are silently skipped.
func Get(key string, files ...string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	for _, path := range files {
		if v := value(path, key); v != "" {
			return v
		}
	}
	return ""
}

// value parses a tiny KEY=VALUE .env file (comments and blank lines ignored,
// surrounding quotes stripped) and returns the value for key, or "".
func value(path, key string) string {
	f, err := os.Open(path) // #nosec G304 -- caller-supplied .env candidate paths, read-only
	if err != nil {
		return ""
	}
	defer f.Close() // nolint: read-only file; close error is non-actionable
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
