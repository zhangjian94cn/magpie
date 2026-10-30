package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/beat"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/provider"
)

// Codex's background app-server (the daemon codex sessions attach to)
// builds its model list once, as it starts, and runs on per CODEX_HOME,
// across terminals, with no idle exit. So a change magpie makes to what
// Codex reads at start — wiring it in, taking it out, a new catalog —
// reaches no codex session started after it until the daemon restarts:
// luci (Discord, Codex 0.162) took Codex off magpie, opened codex, which
// started the daemon on Codex's own models, and after wiring Codex in
// again every new codex showed the official list. All observations now
// remain pending refresh until the user finishes tasks and acts in Codex.

// codexRestartAll is what is said after a change while copies of Codex
// run that magpie leaves to the user.
var codexRestartAll = noticeCodexBehind.say("command", provider.CodexDaemonRestart)

// keepCodexDaemon is provider.KeepCodexDaemonCurrent; a var so tests can
// see what is asked of it without a codex.
var keepCodexDaemon = provider.KeepCodexDaemonCurrent

// codexDaemonSeen is the last check of this machine's daemon, for the
// Agents row where its processes aren't listed on each look (Windows,
// where listing them is a PowerShell run).
var codexDaemonSeen struct {
	sync.Mutex
	at    time.Time
	check provider.CodexDaemonCheck
}

// codexDaemonCheck checks the daemon of the Codex whose config is path
// against when magpie last changed what Codex reads at start.
func codexDaemonCheck(path string) provider.CodexDaemonCheck {
	a := &Agent{ID: "codex", Path: path}
	_, files, reads := a.startsWith()
	changed := a.changedAt(files, reads)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c := keepCodexDaemon(ctx, filepath.Dir(path), changed)
	if c.Err != nil {
		log.Println("codex app-server:", c.Err)
	}
	codexDaemonSeen.Lock()
	codexDaemonSeen.at, codexDaemonSeen.check = time.Now(), c
	codexDaemonSeen.Unlock()
	return c
}

// codexDaemonNotice is what is said of the daemon after a change: ok is
// false when there is none behind it.
func codexDaemonNotice(path string) (string, bool) {
	c := codexDaemonCheck(path)
	if c.Behind {
		return noticeCodexBehind.say("command", provider.CodexDaemonRestart), true
	}
	return "", false
}

// codexDaemonEvery is how often KeepCodexDaemonCurrent looks while the
// daemon is behind, for the codex sessions on it to end.
const codexDaemonEvery = 15 * time.Second

// KeepCodexDaemonCurrent observes pending refresh until ctx ends. It looks again when magpie writes a file (what Codex
// reads is among them), every so often while the daemon is behind, and
// otherwise only now and then, for another process's write (#3185: a
// look every 15s woke an idle magpie).
func KeepCodexDaemonCurrent(ctx context.Context) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(here(home).codexHome(), "config.toml")
	every, written := codexDaemonEvery, filememo.Written()
	for {
		select {
		case <-ctx.Done():
			return
		case <-written:
		case <-beat.Next(every):
		}
		// taken before the look, so a write made during it isn't missed
		written = filememo.Written()
		var c provider.CodexDaemonCheck
		if _, err := os.Stat(path); err == nil {
			c = daemonCheck(path)
		}
		every = codexDaemonNext(c)
	}
}

// daemonCheck is codexDaemonCheck; a test stands in for it.
var daemonCheck = codexDaemonCheck

// codexDaemonNext is how long KeepCodexDaemonCurrent waits, with nothing
// written, after a look that found c.
func codexDaemonNext(c provider.CodexDaemonCheck) time.Duration {
	if c.Behind && !c.Restarted {
		return codexDaemonEvery
	}
	return filememo.OthersEvery()
}

// CodexDaemonRestarted forgets the daemon last found behind, once the user
// has restarted it.
func CodexDaemonRestarted() {
	codexDaemonSeen.Lock()
	codexDaemonSeen.check = provider.CodexDaemonCheck{}
	codexDaemonSeen.Unlock()
}

// codexDaemonBehind is the daemon as last checked, while that check is
// recent: a copy left on the old list, for StaleCopies where processes
// aren't listed on each look.
func codexDaemonBehind() []StaleCopy {
	codexDaemonSeen.Lock()
	defer codexDaemonSeen.Unlock()
	c := codexDaemonSeen.check
	if !c.Behind || c.Restarted || time.Since(codexDaemonSeen.at) > 3*codexDaemonEvery {
		return nil
	}
	return []StaleCopy{{Kind: "daemon", Since: c.Since}}
}

// The same pending-refresh notice is used with and without attached clients.
var noticeCodexBehind = newNotice("Codex refresh is pending. Finish all Codex tasks, then refresh the app or run {command} yourself; magpie did not restart it.")
