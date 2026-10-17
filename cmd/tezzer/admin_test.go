package main

import (
	"testing"
	"time"
)

func TestFormatAgo(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"negative clamps to 0s", -5 * time.Second, "0s"},
		{"zero", 0, "0s"},
		{"seconds", 59 * time.Second, "59s"},
		{"just under a minute", 59*time.Second + 999*time.Millisecond, "59s"},
		{"exactly a minute", 60 * time.Second, "1m"},
		{"minutes", 5 * time.Minute, "5m"},
		{"just under an hour", 59*time.Minute + 59*time.Second, "59m"},
		{"exactly an hour", time.Hour, "1h"},
		{"hours", 5 * time.Hour, "5h"},
		{"just under a day", 23*time.Hour + 59*time.Minute, "23h"},
		{"exactly a day", 24 * time.Hour, "1d"},
		{"just over a day", 25 * time.Hour, "1d"},
		{"days", 3 * 24 * time.Hour, "3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatAgo(tt.d); got != tt.want {
				t.Errorf("formatAgo(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}
