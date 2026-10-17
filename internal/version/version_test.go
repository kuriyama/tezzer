package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	oldV, oldC, oldB := Version, GitCommit, BuildTime
	t.Cleanup(func() { Version, GitCommit, BuildTime = oldV, oldC, oldB })
	Version, GitCommit, BuildTime = "0.1.2", "88800e4f", "2026-09-30_01:02:03_UTC"

	got := String("tezzer")
	want := "tezzer 0.1.2 (commit 88800e4f, built 2026-09-30_01:02:03_UTC, " + runtime.Version()
	if !strings.HasPrefix(got, want) {
		t.Errorf("String() = %q, want prefix %q", got, want)
	}
	if !strings.HasSuffix(got, runtime.GOOS+"/"+runtime.GOARCH+")") {
		t.Errorf("String() = %q, want GOOS/GOARCH suffix", got)
	}
}
