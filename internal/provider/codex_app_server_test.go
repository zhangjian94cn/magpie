package provider

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// codexAppStub stands in for Codex's provider, the running app-servers
// and their ends, for codexAppStep.
type codexAppStub struct {
	on, known bool
	// route: where Codex's config sends it, beside on
	route    string
	running  []int
	listErr  error
	endErr   error
	turnOpen bool
	ended    []int
	// next: the app-server the app starts after one is ended
	next int
}

func stubCodexApp(t *testing.T) *codexAppStub {
	t.Helper()
	s := &codexAppStub{known: true, next: 100}
	oldOn, oldList, oldEnd, oldTurn := CodexAppRoute, codexAppServersOn, endCodexAppServer, codexTurnRunning
	t.Cleanup(func() {
		CodexAppRoute, codexAppServersOn, endCodexAppServer, codexTurnRunning = oldOn, oldList, oldEnd, oldTurn
		codexApp.Lock()
		codexApp.seen, codexApp.route, codexApp.stale, codexApp.stuck, codexApp.switched = false, "", nil, false, false
		codexApp.Unlock()
	})
	codexApp.Lock()
	codexApp.seen, codexApp.route, codexApp.stale, codexApp.stuck, codexApp.switched = false, "", nil, false, false
	codexApp.Unlock()
	CodexAppRoute = func() (string, bool, bool) {
		r := s.route
		if s.on {
			r += "|out"
		}
		return r, s.on, s.known
	}
	codexAppServersOn = func(context.Context, string) ([]int, error) {
		if s.listErr != nil {
			return nil, s.listErr
		}
		return slices.Clone(s.running), nil
	}
	endCodexAppServer = func(pid int) error {
		if s.endErr != nil {
			return s.endErr
		}
		s.ended = append(s.ended, pid)
		// the app starts another app-server, which reads the provider anew
		if i := slices.Index(s.running, pid); i >= 0 {
			s.running[i] = s.next
			s.next++
		}
		return nil
	}
	codexTurnRunning = func(string, time.Time, time.Time) bool { return s.turnOpen }
	return s
}

// Provider changes remain pending until Codex itself replaces the old process.
func TestCodexAppServerPendingRefresh(t *testing.T) {
	signIn(t)
	s := stubCodexApp(t)
	s.running = []int{60}
	now := time.Now()
	idle := func() (int, time.Time) { return 0, time.Time{} }
	step := func() { codexAppStep(context.Background(), now, idle) }
	step()
	if CodexAppStuck() {
		t.Fatal("unchanged account is pending")
	}
	for _, route := range []string{"connected", "", "connected-again"} {
		s.route = route
		step()
		if !CodexAppStuck() || len(s.ended) != 0 {
			t.Fatalf("route %q: pending=%v ended=%v", route, CodexAppStuck(), s.ended)
		}
	}
	// Neither guessed idle time nor repeated observations may retry termination.
	for range 10 {
		step()
	}
	if len(s.ended) != 0 {
		t.Fatalf("ended %v", s.ended)
	}
	s.running = []int{70}
	step()
	if CodexAppStuck() {
		t.Fatal("user-refreshed process is still pending")
	}
}

func TestCodexAppServerUnknown(t *testing.T) {
	signIn(t)
	s := stubCodexApp(t)
	s.running, s.on, s.known = []int{60}, true, false
	step := func() { codexAppStep(context.Background(), time.Now(), nil) }
	step()
	s.known = true
	s.listErr = errors.New("ps failed")
	step()
	s.listErr = nil
	step()
	if !CodexAppStuck() || len(s.ended) != 0 {
		t.Fatalf("pending=%v ended=%v", CodexAppStuck(), s.ended)
	}
	s.running = nil
	s.listErr = errors.New("ps failed")
	step()
	if !CodexAppStuck() {
		t.Fatal("failed read cleared pending refresh")
	}
	s.listErr = nil
	step()
	if CodexAppStuck() {
		t.Fatal("confirmed exit did not clear pending refresh")
	}
}

// TestCodexAppHomeHelper is TestCodexAppServersOnHome's app-server: it
// waits to be ended (a system binary's environment is kept from ps).
func TestCodexAppHomeHelper(t *testing.T) {
	if os.Getenv("MAGPIE_CODEX_HOME_HELPER") != "1" {
		return
	}
	os.Stdout.WriteString("running\n")
	time.Sleep(30 * time.Second)
}

// Only the app-servers on magpie's Codex home are restarted: one under
// another HOME or CODEX_HOME (a sandbox, another user) is left be.
func TestCodexAppServersOnHome(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no environment to tell homes by here: every app-server counts")
	}
	start := func(env ...string) int {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexAppHomeHelper$")
		// TMPDIR is this test's: given no testenv.Marker, the helper makes
		// a test home of its own there, which it never removes once killed
		cmd.Env = append([]string{"MAGPIE_CODEX_HOME_HELPER=1", "TMPDIR=" + t.TempDir()}, env...)
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); _ = cmd.Wait() })
		// running, so its exec is done: Start returns before Linux has
		// written the new program's environment, and /proc/<pid>/environ
		// read then is empty (#3906)
		if line, _ := bufio.NewReader(out).ReadString('\n'); line != "running\n" {
			t.Fatalf("the helper: %q", line)
		}
		return cmd.Process.Pid
	}
	ctx := context.Background()
	a, b := t.TempDir(), t.TempDir()
	onHome := start("HOME=" + a)
	if !codexAppHomeIs(ctx, onHome, filepath.Join(a, ".codex")) {
		t.Fatal("HOME's .codex: not counted")
	}
	if codexAppHomeIs(ctx, onHome, filepath.Join(b, ".codex")) {
		t.Fatal("another HOME's: counted")
	}
	onCodexHome := start("HOME="+a, "CODEX_HOME="+b)
	if !codexAppHomeIs(ctx, onCodexHome, b) {
		t.Fatal("CODEX_HOME: not counted")
	}
	if codexAppHomeIs(ctx, onCodexHome, filepath.Join(a, ".codex")) {
		t.Fatal("HOME's .codex under another CODEX_HOME: counted")
	}
	if codexAppHomeIs(ctx, 1<<30, b) {
		t.Fatal("a process that isn't there counted")
	}
}

// A turn is open in Codex's sessions when the newest task event of a file
// written to lately is task_started.
func TestCodexTurnOpen(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "27")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(name, body string, at time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	started := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"1"}}` + "\n"
	done := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"1"}}` + "\n"
	aborted := `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"1"}}` + "\n"
	if codexTurnOpen(home, now, time.Time{}) {
		t.Fatal("no sessions: open")
	}
	write("rollout-a.jsonl", started+done, now)
	if codexTurnOpen(home, now, time.Time{}) {
		t.Fatal("turn complete: open")
	}
	write("rollout-b.jsonl", started+aborted, now)
	if codexTurnOpen(home, now, time.Time{}) {
		t.Fatal("turn aborted: open")
	}
	write("rollout-c.jsonl", started+done+started, now.Add(-codexTurnWindow-time.Minute))
	if codexTurnOpen(home, now, time.Time{}) {
		t.Fatal("a turn left open long ago: open")
	}
	write("rollout-d.jsonl", started+done+started, now)
	if !codexTurnOpen(home, now, time.Time{}) {
		t.Fatal("a turn running: not open")
	}
	// since: a turn with no readable start still counts
	if !codexTurnOpen(home, now, now.Add(-time.Hour)) {
		t.Fatal("a turn running whose start can't be read: not open")
	}
	if err := os.Remove(filepath.Join(dir, "rollout-d.jsonl")); err != nil {
		t.Fatal(err)
	}
	stamped := func(at time.Time) string {
		return `{"timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `","type":"event_msg","payload":{"type":"task_started","turn_id":"2"}}` + "\n"
	}
	since := now.Add(-time.Minute)
	write("rollout-e.jsonl", started+done+stamped(since.Add(-time.Hour)), now)
	if codexTurnOpen(home, now, since) {
		t.Fatal("a turn started before the app-servers: open")
	}
	if !codexTurnOpen(home, now, time.Time{}) {
		t.Fatal("a turn started an hour ago, since unknown: not open")
	}
	write("rollout-e.jsonl", started+done+stamped(since.Add(-time.Second)), now)
	if !codexTurnOpen(home, now, since) {
		t.Fatal("a turn started within the slack of the app-servers' start: not open")
	}
	write("rollout-e.jsonl", started+done+stamped(since.Add(time.Second)), now)
	if !codexTurnOpen(home, now, since) {
		t.Fatal("a turn started after the app-servers: not open")
	}
}

// A turn left open by an app-server that is gone doesn't hold the switch
// back. 蛋仔 (danzi002 on Discord, 10-29 and 10-30) switched the Codex
// app's used-up account in magpie and the app stayed grey until it was
// quit and opened: "点了会话再切换账号就重启不了". Codex 0.164's
// app-server, killed mid-turn (force quit, crash), writes no turn_aborted,
// and opening that thread in a new app-server writes
// thread_settings_applied, so the session file read as a turn running for
// as long as the user had it open. A turn started before the app-servers
// to be ended can't be running in them; one started after still holds
// them, and so does any open turn when their start can't be read. The
// lines are the shape Codex 0.164.0-alpha.11.1 wrote for a SIGKILLed
// app-server and a resume, payloads cut short.
func TestCodexAppSwitchPastATurnItsServerNeverRan(t *testing.T) {
	signIn(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	s := stubCodexApp(t)
	codexTurnRunning = func(home string, now, since time.Time) bool { return codexTurnOpen(home, now, since) }
	oldStart := codexServerStart
	t.Cleanup(func() {
		codexServerStart = oldStart
		DismissCodexDaemon()
		DismissCodexApp()
	})
	now := time.Now()
	serverStart := now.Add(-5 * time.Minute)
	startErr := error(nil)
	codexServerStart = func(pid int) (time.Time, error) { return serverStart, startErr }
	dir := filepath.Join(home, "sessions", "2026", "10", "30")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(at time.Time, n int, typ, payload string) string {
		return `{"timestamp":"` + at.UTC().Format("2006-01-02T15:04:05.000Z") + `","ordinal":` + strconv.Itoa(n) + `,"type":"` + typ + `","payload":` + payload + "}\n"
	}
	rollout := func(turnAt time.Time) {
		body := line(turnAt, 0, "session_meta", `{"session_id":"01a188a5-f780-7dc3-92a8-919bf6116f56"}`) +
			line(turnAt, 1, "event_msg", `{"type":"task_started","turn_attribution":{"turn_id":"01a188a5-f791-7341-aee6-b832a5a5ec87"}}`) +
			line(turnAt, 7, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`) +
			line(turnAt, 8, "event_msg", `{"type":"item_completed","thread_id":"01a188a5-f780-7dc3-92a8-919bf6116f56"}`) +
			// the thread opened again in the app's next app-server
			line(now.Add(-time.Second), 9, "event_msg", `{"type":"thread_settings_applied","thread_id":"01a188a5-f780-7dc3-92a8-919bf6116f56"}`)
		if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-30T00-01-00-01a188a5.jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idle := func() (int, time.Time) { return 0, now.Add(-time.Minute) }
	step := func() []int { return codexAppStep(context.Background(), now, idle) }

	s.running = []int{60}
	step()
	rollout(serverStart.Add(time.Minute))
	noteCodexSwitch("a@x", "b@x")
	if got := step(); got != nil {
		t.Fatalf("a turn started in the running app-server: ended %v", got)
	}
	rollout(serverStart.Add(-10 * time.Minute))
	startErr = errors.New("gone")
	if got := step(); got != nil {
		t.Fatalf("an open turn with the app-server's start unknown: ended %v", got)
	}
	startErr = nil
	if got := step(); len(got) != 0 || len(s.ended) != 0 || !CodexAppStuck() {
		t.Fatalf("a past turn must not authorize restart: ended=%v calls=%v pending=%v", got, s.ended, CodexAppStuck())
	}
}

// While the account Codex is on is near out, or out, it is read every
// minute, not five: the app greys Send on its own reading, and magpie
// takes over only on its own (Ethereal499). At the reset, as soon.
func TestCodexLookEvery(t *testing.T) {
	signIn(t) // me@example.com, active
	rememberLogins(true)
	at := func(used float64, held bool) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{"me@example.com": {Held: held, Windows: []QuotaWindow{{Name: "5h", Used: 20}, {Name: "week", Used: used}}}}
	}
	if got := codexLookEvery(at(50, false)); got != loginSwitchEvery {
		t.Fatalf("half used: %v", got)
	}
	if got := codexLookEvery(at(91, false)); got != codexNearOutEvery {
		t.Fatalf("near out: %v", got)
	}
	if got := codexLookEvery(at(0, true)); got != codexNearOutEvery {
		t.Fatalf("out: %v", got)
	}
	if got := codexLookEvery(map[string]SubscriptionQuota{"other@example.com": {Held: true}}); got != loginSwitchEvery {
		t.Fatalf("another account out: %v", got)
	}
	if got := codexLookEvery(map[string]SubscriptionQuota{"me@example.com": {Held: true, Error: "x"}}); got != loginSwitchEvery {
		t.Fatalf("a failed read: %v", got)
	}
}

// The Agents row says until when Codex is on magpie: the latest reset of
// the windows used up.
func TestCodexOutUntil(t *testing.T) {
	signIn(t)
	rememberLogins(true)
	soon, later := time.Now().Add(time.Hour), time.Now().Add(48*time.Hour)
	u := map[string]SubscriptionQuota{"me@example.com": {Held: true, Windows: []QuotaWindow{
		{Name: "5h", Used: 100, ResetsAt: &soon},
		{Name: "week", Used: 100, ResetsAt: &later},
	}}}
	if got := codexOutUntil(u); !got.Equal(later) {
		t.Fatalf("both used up: %v, want %v", got, later)
	}
	u["me@example.com"].Windows[1].Used = 40
	if got := codexOutUntil(u); !got.Equal(soon) {
		t.Fatalf("5h used up: %v, want %v", got, soon)
	}
	q := u["me@example.com"]
	q.Held = false
	u["me@example.com"] = q
	if got := codexOutUntil(u); !got.IsZero() {
		t.Fatalf("not out: %v", got)
	}
}

// codexWindowsAppServer is the Windows Codex app's own app-server.
const codexWindowsAppServer = `C:\Users\yeton\AppData\Local\OpenAI\Codex\bin\2993767bd1dfb009\codex.exe -c features.code_mode_host=true app-server --analytics-default-enabled -c plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled=true`

// Only the app's own app-server is restarted: another program's, started
// from the app's bundled codex with --listen (Miyi Helper's, seen on the
// owner's Mac), is that program's.
func TestCodexAppOwnServer(t *testing.T) {
	for _, own := range []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex -c features.code_mode_host=true app-server --analytics-default-enabled -c plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled=true",
		// the Windows app's (OpenAI.Codex 26.1021, ChatGPT.exe's child),
		// as Windows listed it on the Windows box: run from LOCALAPPDATA,
		// not from its package (zhibukaixinyao on Discord)
		codexWindowsAppServer,
	} {
		if !isCodexAppOwnServer(own) {
			t.Errorf("the app's own app-server not taken: %s", own)
		}
	}
	for _, other := range []string{
		`C:\Users\me\AppData\Local\OpenAI\Codex\bin\2993767bd1dfb009\codex.exe app-server --listen stdio://`,
		`C:\Users\me\AppData\Roaming\npm\node_modules\@openai\codex\vendor\x86_64-pc-windows-msvc\codex\codex.exe app-server`,
		`C:\Users\me\AppData\Local\Microsoft\WinGet\Links\codex.exe app-server`,
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex app-server --listen stdio://",
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex app-server --listen=ws://127.0.0.1:4500",
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex -c forced_login_method=\"chatgpt\" exec-server --remote https://codex-cloud-environments.chatgpt.com/api",
		"/usr/local/bin/codex app-server",
	} {
		if isCodexAppOwnServer(other) {
			t.Errorf("taken for the app's own app-server: %s", other)
		}
	}
}

// Account switches only mark the app pending, even when session reads say idle.
func TestCodexAppServerPendingOnAccountSwitch(t *testing.T) {
	signIn(t)
	s := stubCodexApp(t)
	s.running = []int{60}
	step := func() { codexAppStep(context.Background(), time.Now(), nil) }
	step()
	codexAppAccountSwitched()
	step()
	if !CodexAppStuck() || len(s.ended) != 0 {
		t.Fatalf("pending=%v ended=%v", CodexAppStuck(), s.ended)
	}
}

func TestRestartCodexAppWhenIdle(t *testing.T) {
	s := stubCodexApp(t)
	if n, left, err := RestartCodexAppWhenIdle(context.Background(), time.Second); n != 0 || left || err != nil {
		t.Fatalf("no app: %d %v %v", n, left, err)
	}
	s.running = []int{7, 8}
	for _, open := range []bool{true, false} {
		s.turnOpen = open
		if n, left, err := RestartCodexAppWhenIdle(context.Background(), time.Second); n != 0 || !left || err != nil || len(s.ended) != 0 {
			t.Fatalf("turn open=%v: ended=%v (%d), pending=%v, %v", open, s.ended, n, left, err)
		}
	}
	s.listErr = errors.New("ps failed")
	if n, left, err := RestartCodexAppWhenIdle(context.Background(), 0); n != 0 || !left || err == nil {
		t.Fatalf("failed read: %d %v %v", n, left, err)
	}
}

// codexMacAppServer is the macOS Codex app's own app-server, as ps listed
// it on the owner's Mac (ChatGPT.app 26.1027).
const codexMacAppServer = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex -c features.code_mode_host=true app-server --analytics-default-enabled -c plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled=true"
