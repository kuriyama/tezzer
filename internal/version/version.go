package version

// Variables injected at build time via ldflags.
var (
	// GitCommit is the git commit hash of the build.
	GitCommit = "unknown"
	// BuildTime is the build timestamp.
	BuildTime = "unknown"
)

// GetVersion returns the version information.
func GetVersion() string {
	return GitCommit
}

// GetBuildTime returns the build timestamp.
func GetBuildTime() string {
	return BuildTime
}
