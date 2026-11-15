// Package version holds the single hoard version number, shown in both web
// UIs and by -version. Bump the VERSION file for a release; the Docker image
// tag and the Nix package version follow it.
package version

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var raw string

// Version is the release number, e.g. "0.28".
var Version = strings.TrimSpace(raw)
