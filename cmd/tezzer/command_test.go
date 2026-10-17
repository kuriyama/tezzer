package main

import (
	"reflect"
	"testing"
)

func TestResolveCommand(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		args     []string
		wantCmd  string
		wantArgs []string
	}{
		{"nothing given: login shell", "", nil, "", nil},
		{"command after --", "", []string{"tmux", "new", "-A"}, "tmux", []string{"new", "-A"}},
		{"command only after --", "", []string{"zsh"}, "zsh", []string{}},
		{"-cmd with args (tezzer-ssh form)", "tmux", []string{"a"}, "tmux", []string{"a"}},
		{"-cmd without args", "zsh", nil, "zsh", nil},
		{"-cmd /bin/bash keeps args as args", "/bin/bash", []string{"-l"}, "/bin/bash", []string{"-l"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCmd, gotArgs := resolveCommand(tt.cmd, tt.args)
			if gotCmd != tt.wantCmd || !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("resolveCommand(%q, %q) = %q, %q; want %q, %q",
					tt.cmd, tt.args, gotCmd, gotArgs, tt.wantCmd, tt.wantArgs)
			}
		})
	}
}

func TestIgnoredCommandNotice(t *testing.T) {
	if got := ignoredCommandNotice("work", "zellij", "", nil); got != "" {
		t.Errorf("no command given: got %q, want empty", got)
	}
	// 作成時と同じコマンドで入り直す（tezzer-ssh host run --name w -- zsh の再実行）
	if got := ignoredCommandNotice("w", "zsh", "zsh", nil); got != "" {
		t.Errorf("same command: got %q, want empty", got)
	}
	// 引数つきは既存セッションの引数と比較できないので知らせる
	if got := ignoredCommandNotice("w", "tmux", "tmux", []string{"new"}); got == "" {
		t.Error("same command with args: got empty, want notice")
	}
	got := ignoredCommandNotice("A-backend", "zellij", "zsh", nil)
	want := "Session \"A-backend\" already exists (running zellij); the given command \"zsh\" is not used\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = ignoredCommandNotice("w", "/bin/bash", "tmux", []string{"new", "-A"})
	if want := "Session \"w\" already exists (running /bin/bash); the given command \"tmux new -A\" is not used\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
