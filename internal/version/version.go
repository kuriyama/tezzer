package version

// ビルド時にldflags経由で埋め込まれる変数
var (
	// GitCommit はビルド時のgit commit hash
	GitCommit = "unknown"
	// BuildTime はビルド時刻
	BuildTime = "unknown"
)

// GetVersion はバージョン情報を返す
func GetVersion() string {
	return GitCommit
}

// GetBuildTime はビルド時刻を返す
func GetBuildTime() string {
	return BuildTime
}
