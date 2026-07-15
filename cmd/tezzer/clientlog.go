package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// clientLogDir はクライアントログの置き場所（~/.tezzer/logs）を返す。
func clientLogDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home dir: %w", err)
	}
	return filepath.Join(homeDir, ".tezzer", "logs"), nil
}

// openClientLogFile は ~/.tezzer/logs/ にクライアントログファイルを作成して返す。
// 起動時に古いログファイル（7日以上前）を自動削除する。
// エラーが発生してもプロセスは続行するため、呼び出し元はエラーを警告のみに使う。
func openClientLogFile() (*os.File, error) {
	logDir, err := clientLogDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create log dir: %w", err)
	}

	cleanOldClientLogs(logDir, 7*24*time.Hour)

	logPath := filepath.Join(logDir, fmt.Sprintf("client-%s-%d.log",
		time.Now().Format("20060102-150405"), os.Getpid()))

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", logPath, err)
	}
	return f, nil
}

// createSessionLogSymlink は logDir 内に session-<id>.log → 実ログファイルのシンボリックリンクを作る。
// 既存の古いリンクがあれば上書きする。エラーは無視して続行してよい。
func createSessionLogSymlink(sessionID string, logFile *os.File) {
	if logFile == nil {
		return
	}
	linkPath, err := sessionLogPath(sessionID)
	if err != nil {
		return
	}
	_ = os.Remove(linkPath)
	_ = os.Symlink(logFile.Name(), linkPath)
}

// sessionLogPath は session-<id>.log シンボリックリンクのパスを返す。
func sessionLogPath(sessionID string) (string, error) {
	logDir, err := clientLogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(logDir, "session-"+sessionID+".log"), nil
}

// cleanOldClientLogs は logDir 内の maxAge より古いファイルとシンボリックリンクを削除する。
func cleanOldClientLogs(logDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(logDir, entry.Name()))
		}
	}
}

// showSessionLogs は session-<id>.log シンボリックリンク経由でログを標準出力に出力する。
func showSessionLogs(sessionID string) error {
	linkPath, err := sessionLogPath(sessionID)
	if err != nil {
		return err
	}
	f, err := os.Open(linkPath)
	if err != nil {
		return fmt.Errorf("log not found for session %s (connect once to create): %w", sessionID, err)
	}
	defer f.Close()

	resolved, _ := filepath.EvalSymlinks(linkPath)
	if resolved != "" {
		fmt.Printf("# %s\n", resolved)
	}
	_, err = io.Copy(os.Stdout, f)
	return err
}

// fileLogger はログファイルへの専用ロガー（TEZZER_DEBUG に依存しない常時書き出し用）。
type fileLogger struct {
	l *log.Logger
}

func newFileLogger(f *os.File) *fileLogger {
	return &fileLogger{l: log.New(f, "", log.LstdFlags)}
}

func (fl *fileLogger) Printf(format string, args ...interface{}) {
	if fl != nil {
		fl.l.Printf(format, args...)
	}
}

func (fl *fileLogger) Print(msg string) {
	if fl != nil {
		fl.l.Print(msg)
	}
}
