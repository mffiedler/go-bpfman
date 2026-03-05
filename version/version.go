// Package version holds build-time version information injected via
// ldflags. The Makefile sets these using -X linker flags.
package version

import (
	"fmt"
	"runtime"
	"strings"
)

// These variables are set at build time via -ldflags -X.
var (
	gitCommit string // full git commit hash
	gitBranch string // git branch name
	gitState  string // "clean" or "dirty"
	buildDate string // ISO 8601 build timestamp
	version   string // semantic version tag, if any
)

// Info contains structured version information.
type Info struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	GitBranch string `json:"git_branch"`
	GitState  string `json:"git_state"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the current build version information.
func Get() Info {
	v := version
	if v == "" {
		v = "(devel)"
	}
	return Info{
		Version:   v,
		GitCommit: gitCommit,
		GitBranch: gitBranch,
		GitState:  gitState,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String returns a single-line summary suitable for log output.
func (i Info) String() string {
	var parts []string
	parts = append(parts, i.Version)
	if i.GitCommit != "" {
		short := i.GitCommit
		if len(short) > 12 {
			short = short[:12]
		}
		parts = append(parts, short)
	}
	if i.GitState == "dirty" {
		parts = append(parts, "dirty")
	}
	if i.BuildDate != "" {
		parts = append(parts, i.BuildDate)
	}
	parts = append(parts, i.GoVersion)
	parts = append(parts, i.Platform)
	return strings.Join(parts, " ")
}

// Long returns a multi-line version string for display.
func (i Info) Long() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Version:    %s\n", i.Version)
	fmt.Fprintf(&b, "Git commit: %s\n", i.GitCommit)
	fmt.Fprintf(&b, "Git branch: %s\n", i.GitBranch)
	fmt.Fprintf(&b, "Git state:  %s\n", i.GitState)
	fmt.Fprintf(&b, "Build date: %s\n", i.BuildDate)
	fmt.Fprintf(&b, "Go version: %s\n", i.GoVersion)
	fmt.Fprintf(&b, "Platform:   %s\n", i.Platform)
	return b.String()
}
