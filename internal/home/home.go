// Package home locates hull's data directory.
package home

import (
	"os"
	"path/filepath"
)

// Dir returns hull's data directory: $HULL_HOME, else $XDG_DATA_HOME/hull,
// else ~/.local/share/hull.
func Dir() string {
	if d := os.Getenv("HULL_HOME"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "hull")
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return filepath.Join(os.TempDir(), "hull")
	}
	return filepath.Join(h, ".local", "share", "hull")
}

// Sub returns a subdirectory of the data directory, creating it if needed.
func Sub(parts ...string) (string, error) {
	p := filepath.Join(append([]string{Dir()}, parts...)...)
	return p, os.MkdirAll(p, 0o755)
}
