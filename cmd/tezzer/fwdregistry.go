package main

// fwdregistry.go: このマシンで動いている tezzer クライアントの転送登録簿（-list-forwards）。
// -L はクライアントの持ち物でサーバはポートを知らないため、各クライアントが
// ~/.tezzer/run/<pid>.json に自分の転送指定を書き出し、-list-forwards がそれを集める。
// 生存判定はファイルの flock で行う（クライアントは生存中ずっと排他ロックを保持し、
// ロックが取れたファイルは異常終了の残骸として消す）。PID 再利用に影響されない。
// -R の実体（listen 中か・どの端末が持っているか）はサーバ側にあるため、ここには
// 「このプロセスが要求している」ことだけを書く（実状態は tezzer -list）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/unix"
)

type fwdRegistryEntry struct {
	PID            int            `json:"pid"`
	StartedAt      string         `json:"started_at"`
	Server         string         `json:"server"`
	SessionID      string         `json:"session_id"`
	SessionName    string         `json:"session_name,omitempty"`
	LocalForwards  []fwdRegistryL `json:"local_forwards,omitempty"`
	RemoteForwards []fwdRegistryR `json:"remote_forwards,omitempty"`
}

type fwdRegistryL struct {
	Listen    string `json:"listen"`
	Target    string `json:"target"`
	Listening bool   `json:"listening"`
	BindError string `json:"bind_error,omitempty"`
}

type fwdRegistryR struct {
	Port   uint16 `json:"port"`
	Target string `json:"target"`
}

func fwdRegistryDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tezzer", "run"), nil
}

// registerForwards は entry を登録簿に書き出し、終了時に呼ぶ解除関数を返す。
// 一時ファイルにロックを取って書き終えてから rename するので、-list-forwards が
// 書きかけやロック前のファイルを残骸と誤認して消すことはない（rename でロックは保たれる）。
func registerForwards(entry fwdRegistryEntry) (func(), error) {
	dir, err := fwdRegistryDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return nil, err
	}
	base := strconv.Itoa(entry.PID)
	tmp := filepath.Join(dir, "."+base+".tmp")
	path := filepath.Join(dir, base+".json")
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fail(err)
	}
	return func() {
		_ = os.Remove(path)
		_ = f.Close()
	}, nil
}

// readForwardRegistry は生きているクライアントの登録を PID 順に返す。
// ロックが取れた（＝持ち主が死んでいる）ファイルは消す。
func readForwardRegistry() ([]fwdRegistryEntry, error) {
	dir, err := fwdRegistryDir()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var entries []fwdRegistryEntry
	for _, p := range paths {
		if e, ok := readLiveEntry(p); ok {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].PID < entries[j].PID })
	return entries, nil
}

func readLiveEntry(path string) (fwdRegistryEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		return fwdRegistryEntry{}, false
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		// 持ち主がいない残骸
		removeStale(f, path)
		return fwdRegistryEntry{}, false
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		return fwdRegistryEntry{}, false
	}
	var e fwdRegistryEntry
	if err := json.NewDecoder(f).Decode(&e); err != nil {
		return fwdRegistryEntry{}, false
	}
	return e, true
}

// removeStale は開いている残骸 f が今も path にあるときだけ path を消す。open から flock までの間に
// PID が再利用され、新しいクライアントが同じ名前へ rename していた場合に、その登録を消さないため。
func removeStale(f *os.File, path string) {
	fi, err := f.Stat()
	if err != nil {
		return
	}
	cur, err := os.Stat(path)
	if err != nil || !os.SameFile(fi, cur) {
		return
	}
	_ = os.Remove(path)
}

// newForwardRegistryEntry は Client の転送指定と -L の bind 結果（c.forwards と同順）から
// 登録内容を作る。server は表示用（tezzer-ssh 経由なら接続先ホスト、それ以外はソケットパス）。
func newForwardRegistryEntry(c *Client, server string, bindErrs []error) fwdRegistryEntry {
	e := fwdRegistryEntry{
		PID:         os.Getpid(),
		StartedAt:   time.Now().Format(time.RFC3339),
		Server:      server,
		SessionID:   c.sessionID,
		SessionName: c.sessionName,
	}
	for i, sp := range c.forwards {
		l := fwdRegistryL{Listen: sp.listenAddr, Target: sp.target, Listening: true}
		if i < len(bindErrs) && bindErrs[i] != nil {
			l.Listening = false
			l.BindError = bindErrs[i].Error()
		}
		e.LocalForwards = append(e.LocalForwards, l)
	}
	for _, sp := range c.rforwards {
		e.RemoteForwards = append(e.RemoteForwards, fwdRegistryR{Port: sp.port, Target: sp.target})
	}
	return e
}

// listForwards は -list-forwards の本体。
func listForwards(jsonOut bool) error {
	entries, err := readForwardRegistry()
	if err != nil {
		return err
	}
	if jsonOut {
		if entries == nil {
			entries = []fwdRegistryEntry{}
		}
		out, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	fmt.Print(formatForwardRegistry(entries))
	return nil
}

func formatForwardRegistry(entries []fwdRegistryEntry) string {
	if len(entries) == 0 {
		return "No port forwards on this machine.\n"
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tSERVER\tSESSION\tTYPE\tLISTEN\tTARGET\tSTATE")
	for _, e := range entries {
		sess := shortSessionID(e.SessionID)
		if e.SessionName != "" {
			sess = e.SessionName + " (" + sess + ")"
		}
		for _, l := range e.LocalForwards {
			state := "listening"
			if !l.Listening {
				state = "bind failed: " + l.BindError
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t-L\t%s\t%s\t%s\n", e.PID, e.Server, sess, l.Listen, l.Target, state)
		}
		for _, r := range e.RemoteForwards {
			fmt.Fprintf(w, "%d\t%s\t%s\t-R\tserver:%d\t%s\t%s\n", e.PID, e.Server, sess, r.Port, r.Target, "requested (see tezzer -list)")
		}
	}
	_ = w.Flush()
	return b.String()
}

func shortSessionID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}
