package provider

import (
	"context"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// With Codex on a provider that takes no ChatGPT account (magpie's table
// with requires_openai_auth = false, danzi002 on Discord: "requires_openai_auth
// = false 的时候就不应该重启 codex 了"), the app-server sends the
// provider's own key and reads no account at all, so switching the account
// in auth.json changes nothing it goes by: neither a switch nor a switch
// off a spent account restarts the Codex app's app-server, and neither the
// app nor the background daemon is said to be left on the old account. A
// route change still restarts it, and with the provider taking the account
// again a switch does too.
func TestCodexNoAccountSwitchRestartsNothing(t *testing.T) {
	signIn(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	s := stubCodexApp(t)
	old, oldHome, oldNo := listProcesses, processCodexHome, CodexNoAccount
	t.Cleanup(func() {
		listProcesses, processCodexHome, CodexNoAccount = old, oldHome, oldNo
		DismissCodexDaemon()
		DismissCodexApp()
	})
	processCodexHome = func(int) (string, bool) { return "", false }
	listProcesses = func(context.Context) ([]proc.Process, error) {
		ps := []proc.Process{{PID: 9, Args: "codex app-server --managed-daemon"}}
		for _, p := range s.running {
			ps = append(ps, proc.Process{PID: p, Args: codexWindowsAppServer})
		}
		return ps, nil
	}
	noAccount := true
	CodexNoAccount = func() bool { return noAccount }
	now := time.Now()
	idle := func() (int, time.Time) { return 0, now.Add(-time.Minute) }
	step := func() []int { return codexAppStep(context.Background(), now, idle) }
	recheck := func() {
		codexDaemonMu.Lock()
		codexDaemonChecked = time.Now()
		codexDaemonMu.Unlock()
	}

	s.route, s.running = "magpie,noauth", []int{60}
	if got := step(); got != nil {
		t.Fatalf("nothing changed: ended %v", got)
	}
	noteCodexSwitch("a@x", "b@x")
	recheck()
	for range 3 {
		if got := step(); got != nil {
			t.Fatalf("switched on a provider that takes no account: ended %v", got)
		}
	}
	if got := CodexAppStale(); got != "" {
		t.Fatalf("the Codex app said to be left on %q", got)
	}
	if got := CodexDaemonStale(); got != "" {
		t.Fatalf("the daemon said to be left on %q", got)
	}
	codexAppSpentSwitch()
	if got := step(); got != nil {
		t.Fatalf("switched off a spent account on a provider that takes no account: ended %v", got)
	}
	// where Codex sends its requests changed: under the pending-refresh
	// boundary nothing is ended; the app-server is marked pending refresh
	s.route = "magpie,auth"
	if got := step(); got != nil {
		t.Fatalf("route changed: ended %v, want none", got)
	}
	if !CodexAppBack() {
		t.Fatal("route changed: the app-server isn't pending refresh")
	}
	// on a provider that takes the account a switch restarts it
	noAccount = false
	noteCodexSwitch("b@x", "c@x")
	recheck()
	// the app-server never restarted (pending-refresh boundary): it is
	// still the one that started on the first account
	if got := CodexAppStale(); got != "a@x" {
		t.Fatalf("on a provider that takes the account: app left on %q, want a@x", got)
	}
	if got := step(); got != nil {
		t.Fatalf("switched on a provider that takes the account: ended %v, want none", got)
	}
	if !CodexAppBack() {
		t.Fatal("switched on a provider that takes the account: the app-server isn't pending refresh")
	}
}
