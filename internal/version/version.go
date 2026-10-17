// Package version holds build information injected at link time.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Variables injected at build time via ldflags.
var (
	// Version is the release version (e.g. "0.1.2"), or a git describe string
	// such as "0.1.1-42-g88800e4-dirty" for development builds.
	Version = "dev"
	// GitCommit is the git commit hash of the build.
	GitCommit = "unknown"
	// BuildTime is the build timestamp.
	BuildTime = "unknown"
)

func init() {
	// make を通さない go build / go install では ldflags が入らないので、
	// Go が埋め込む VCS 情報（リポジトリ内でビルドした場合のみ）から補う。
	if GitCommit != "unknown" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		case "vcs.time":
			if BuildTime == "unknown" {
				BuildTime = s.Value
			}
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if rev != "" {
		GitCommit = rev
		if modified == "true" {
			GitCommit += "-dirty"
		}
	}
}

// GetVersion returns the build ID (the git commit hash).
func GetVersion() string {
	return GitCommit
}

// GetBuildTime returns the build timestamp.
func GetBuildTime() string {
	return BuildTime
}

// String returns a one-line version description for -version, e.g.
// "tezzer 0.1.2 (commit 88800e4, built 2026-09-30_01:02:03_UTC, go1.25.12 linux/amd64)".
func String(program string) string {
	return fmt.Sprintf("%s %s (commit %s, built %s, %s %s/%s)",
		program, Version, GitCommit, BuildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
