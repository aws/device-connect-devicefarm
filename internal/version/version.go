// Package version holds the release version embedded from the VERSION file.
package version

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var raw string

// String returns the embedded version, vX.Y.Z, with the file's line ending trimmed.
func String() string {
	return strings.TrimSpace(raw)
}
