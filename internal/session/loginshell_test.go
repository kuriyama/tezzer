package session

import (
	"os"
	"testing"
	"time"
)

func TestUseLoginShell(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	mgr := NewManager()
	defer mgr.Close()
	sess, err := mgr.CreateSession("", "/bin/sh", []string{"-c", `echo "argv0=$0 cwd=$(pwd)"`}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess.UseLoginShell()
	client := sess.AttachClient(0, "TEST", "c", 0)
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	// ssh と同じく argv[0] が "-sh"、作業ディレクトリが $HOME になる
	waitForEcho(t, client.OutCh, "argv0=-sh cwd="+home, 3*time.Second)
}
