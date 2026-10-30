package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// Codex owns its shared execution backend. Configuration changes leave the
// running app pending refresh; neither request timing nor a session-log
// sample can authorize ending a process that may still run tools.
var errCodexRefreshRequired = errors.New("Codex refresh is pending: finish all Codex tasks, then refresh or restart from Codex; magpie did not restart it")

// CodexAppRoute is set by the agent package: what in Codex's config the
// app-server reads as it starts and magpie writes (its provider, base URL
// and model list: route, compared, never parsed), and whether magpie is
// Codex's provider for its used-up ChatGPT account (out, AppOut); known
// false when Codex's config can't be read. The app's own model pick is not
// a configuration change made by magpie.
var CodexAppRoute func() (route string, out, known bool)

// CodexAppOpen is read-only status for account notices, not a restart hook.
var CodexAppOpen func(ctx context.Context) (open, known bool)

var codexAppEvery = 3 * time.Second

// codexTurnWindow is how long a session file written to last counts as a
// turn maybe still running when it has one started and not ended: a turn
// whose app-server died leaves one open for good.
const codexTurnWindow = 30 * time.Minute

// codexTurnSlack is how much earlier than the app-servers' start a turn's
// timestamp may be and still count as theirs: the two clocks are read by
// different programs.
const codexTurnSlack = 2 * time.Second

// codexAppListWait bounds one listing of the app-servers and their homes:
// at a load of 200 a 10s bound ran out on the owner's Mac (the change was
// taken up at the next look, 9s later).
const codexAppListWait = 30 * time.Second

// codexAppServersOn are the pids of the Codex app's own app-servers that
// run on the Codex home home; vars for tests.
var (
	codexAppServersOn = func(ctx context.Context, home string) ([]int, error) {
		ps, err := listProcesses(ctx)
		if err != nil {
			return nil, err
		}
		var out []int
		for _, p := range ps {
			if isCodexAppOwnServer(p.Args) && codexAppHomeIs(ctx, p.PID, home) {
				out = append(out, p.PID)
			}
		}
		return out, nil
	}
	endCodexAppServer = func(int) error { return errCodexRefreshRequired }
	codexTurnRunning  = func(home string, now, since time.Time) bool { return codexTurnOpen(home, now, since) }
	codexServerStart  = proc.Started
)

// codexServersSince is when the oldest of the app-servers pids started: a
// turn started before it can't be running in any of them. It is zero,
// "unknown", when one's start can't be read, and every open turn counts.
func codexServersSince(pids []int) time.Time {
	var since time.Time
	for _, p := range pids {
		t, err := codexServerStart(p)
		if err != nil || t.IsZero() {
			return time.Time{}
		}
		if since.IsZero() || t.Before(since) {
			since = t
		}
	}
	return since
}

// isCodexAppOwnServer is whether a command line is the Codex app's own
// app-server, which it talks to over stdio: one another program starts
// from the app's bundled codex names where it listens (`app-server
// --listen stdio://`, Miyi Helper on macOS), and is that program's to end.
func isCodexAppOwnServer(args string) bool {
	if !isCodexAppServer(args) {
		return false
	}
	_, rest := splitExe(args)
	for _, f := range strings.Fields(rest) {
		if f == "--listen" || strings.HasPrefix(f, "--listen=") {
			return false
		}
	}
	return true
}

// codexAppHomeIs is whether app-server pid runs on Codex home home: its
// CODEX_HOME, else its HOME's .codex. A system that doesn't tell
// (Windows) counts as yes: one Codex home is the rule there.
func codexAppHomeIs(ctx context.Context, pid int, home string) bool {
	env, err := proc.EnvOf(ctx, pid, "CODEX_HOME", "HOME")
	if errors.Is(err, proc.ErrNoEnv) {
		return true
	}
	if err != nil {
		return false
	}
	h := env["CODEX_HOME"]
	if h == "" && env["HOME"] != "" {
		h = filepath.Join(env["HOME"], ".codex")
	}
	return h != "" && sameDir(h, home)
}

// sameDir is whether a and b name one folder, through links too.
func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// codexApp is what KeepCodexAppOnItsProvider knows.
var codexApp struct {
	sync.Mutex
	seen  bool
	route string
	// stale: app-servers started before Codex's provider last changed.
	stale map[int]int
	// stuck: a running app-server still needs a user-initiated refresh.
	stuck bool
	// switched: Codex's ChatGPT account changed since the app-servers were
	// last listed (codexAppAccountSwitched)
	switched bool
}

// CodexAppStuck reports pending refresh through the existing status field.
func CodexAppStuck() bool {
	c := &codexApp
	c.Lock()
	defer c.Unlock()
	return c.stuck
}

// codexAppAccountSwitched makes the next observation mark the old account
// pending refresh without restarting the app.
func codexAppAccountSwitched() {
	c := &codexApp
	c.Lock()
	defer c.Unlock()
	c.switched = true
}

// A spent account changes the refresh notice, never the lifecycle boundary.
func codexAppSpentSwitch() { codexAppAccountSwitched() }

// KeepCodexAppOnItsProvider observes pending refresh until ctx ends.
// busy is retained for caller compatibility, never used to authorize a restart.
func KeepCodexAppOnItsProvider(ctx context.Context, busy func() (int, time.Time)) {
	t := time.NewTicker(codexAppEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			codexAppStep(ctx, now, busy)
		}
	}
}

// codexAppStep tracks the processes that still need to read changed settings.
// Its legacy return value is always empty: only Codex may restart them.
func codexAppStep(ctx context.Context, _ time.Time, _ func() (int, time.Time)) []int {
	if CodexAppRoute == nil {
		return nil
	}
	route, out, known := CodexAppRoute()
	if !known {
		return nil
	}
	home := codexHome()
	c := &codexApp
	c.Lock()
	// At the first look only a used-up account is known to need refresh.
	changed := !c.seen && out || c.seen && c.route != route || c.switched
	if !changed {
		c.seen, c.route = true, route
	}
	c.Unlock()
	if changed {
		lctx, cancel := context.WithTimeout(ctx, codexAppListWait)
		pids, err := codexAppServersOn(lctx, home)
		cancel()
		if err != nil {
			// unknown: the change is taken up again at the next look
			log.Printf("codex: listing the Codex app's app-servers: %v", err)
			return nil
		}
		c.Lock()
		c.seen, c.route, c.switched = true, route, false
		c.stale, c.stuck = map[int]int{}, false
		for _, p := range pids {
			c.stale[p] = 0
		}
		c.stuck = len(c.stale) > 0
		c.Unlock()
	}
	c.Lock()
	n := len(c.stale)
	c.Unlock()
	if n == 0 {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, codexAppListWait)
	pids, err := codexAppServersOn(lctx, home)
	cancel()
	if err != nil {
		return nil
	}
	running := map[int]bool{}
	for _, p := range pids {
		running[p] = true
	}
	c.Lock()
	for p := range c.stale {
		if !running[p] {
			delete(c.stale, p) // it read the provider afresh as it started
		}
	}
	c.stuck = len(c.stale) > 0
	c.Unlock()
	return nil
}

// codexTurnOpen is diagnostic only: it samples recently written sessions
// for an open turn started at or after since. A zero since counts every
// open turn. A crashed server may leave task_started without turn_aborted;
// neither this sample nor a process's start time authorizes a restart.
func codexTurnOpen(home string, now, since time.Time) bool {
	open := false
	_ = filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || open {
			if open {
				return fs.SkipAll
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > codexTurnWindow {
			return nil
		}
		open = sessionTurnOpen(path, since)
		return nil
	})
	return open
}

// sessionTurnTail is how much of a session file's end sessionTurnOpen reads.
const sessionTurnTail = 512 << 10

// sessionTurnOpen is whether the session file at path ends inside a turn
// started at since or later. A file that can't be read, or a turn whose
// start can't be read, counts as open: a turn cut short costs more than
// an app greyed a few seconds more.
func sessionTurnOpen(path string, since time.Time) bool {
	f, err := os.Open(path)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > sessionTurnTail {
		if _, err := f.Seek(info.Size()-sessionTurnTail, io.SeekStart); err != nil {
			return true
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return true
	}
	started := bytes.LastIndex(b, []byte(`"type":"task_started"`))
	ended := max(bytes.LastIndex(b, []byte(`"type":"task_complete"`)), bytes.LastIndex(b, []byte(`"type":"turn_aborted"`)))
	if started <= ended {
		return false
	}
	if since.IsZero() {
		return true
	}
	at, ok := sessionLineTime(b, started)
	return !ok || !at.Before(since.Add(-codexTurnSlack))
}

// sessionLineTime is the "timestamp" of the session file line holding
// offset i of b, which Codex writes on every rollout line.
func sessionLineTime(b []byte, i int) (time.Time, bool) {
	line := b[bytes.LastIndexByte(b[:i], '\n')+1:]
	if j := bytes.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	var l struct {
		Timestamp time.Time `json:"timestamp"`
	}
	if json.Unmarshal(line, &l) != nil || l.Timestamp.IsZero() {
		return time.Time{}, false
	}
	return l.Timestamp, true
}

// RestartCodexApp keeps old callers safe and explicitly reports no restart.
func RestartCodexApp(context.Context, string) error {
	return errCodexRefreshRequired
}

// RestartSwitchedCodexApp preserves the newer account-switch API without
// ending a running turn or dismissing its pending-refresh notice.
func RestartSwitchedCodexApp(context.Context) error {
	return errCodexRefreshRequired
}

// RestartCodexAppWhenIdle only reports the uninstall notice. It never waits
// for guessed idleness or ends a Codex process; a failed read stays unknown.
func RestartCodexAppWhenIdle(ctx context.Context, _ time.Duration) (ended int, left bool, err error) {
	home := codexHome()
	lctx, cancel := context.WithTimeout(ctx, codexAppListWait)
	pids, err := codexAppServersOn(lctx, home)
	cancel()
	if err != nil {
		return 0, true, err
	}
	return 0, len(pids) > 0, nil
}
