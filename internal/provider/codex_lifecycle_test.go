package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

func TestCodexLifecycleChangesNeverEndAppServers(t *testing.T) {
	signIn(t)
	s := stubCodexApp(t)
	s.running = []int{60001, 60002}
	now := time.Now()
	idle := func() (int, time.Time) { return 0, now.Add(-time.Hour) }
	ctx := context.Background()
	codexAppStep(ctx, now, idle)
	for _, route := range []string{"catalog-v2", "endpoint-v2", "", "connected-again"} {
		s.route = route
		codexAppAccountSwitched()
		for range 5 {
			if ended := codexAppStep(ctx, now, idle); len(ended) != 0 {
				t.Errorf("%q: gateway ended Codex app-servers %v", route, ended)
			}
		}
	}
	if len(s.ended) != 0 || !slices.Equal(s.running, []int{60001, 60002}) {
		t.Fatalf("Codex processes changed: ended=%v running=%v", s.ended, s.running)
	}
	if !CodexAppStuck() {
		t.Fatal("the unchanged Codex app must remain pending refresh")
	}
	s.running = []int{60003}
	codexAppStep(ctx, now, idle)
	if CodexAppStuck() {
		t.Fatal("a user-started replacement must clear pending refresh")
	}
}

func TestCodexLifecycleLongAndUnknownLogsNeverAuthorizeRestart(t *testing.T) {
	for _, fixture := range []string{"long-open", "markerless", "unreadable", "aborted"} {
		t.Run(fixture, func(t *testing.T) {
			signIn(t)
			s := stubCodexApp(t)
			home := t.TempDir()
			path := filepath.Join(home, "sessions", "rollout.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
			body += strings.Repeat(`{"type":"response_item","payload":{"type":"tool_output","text":"pending"}}`+"\n", 16000)
			if fixture == "aborted" {
				body += `{"type":"event_msg","payload":{"type":"turn_aborted"}}` + "\n"
			}
			if fixture == "markerless" {
				body = strings.Repeat(`{"type":"response_item","payload":{}}`+"\n", 16000)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if fixture == "unreadable" {
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			}
			codexTurnRunning = func(_ string, _ time.Time, since time.Time) bool { return sessionTurnOpen(path, since) }
			s.running, s.on = []int{60001}, true
			if ended := codexAppStep(context.Background(), time.Now(), func() (int, time.Time) {
				return 0, time.Now().Add(-time.Hour)
			}); len(ended) != 0 || len(s.ended) != 0 {
				t.Fatalf("%s authorized ending a Codex process: %v %v", fixture, ended, s.ended)
			}
		})
	}
}

func TestCodexLifecycleExplicitAndUninstallPathsNeverEndProcesses(t *testing.T) {
	s := stubCodexApp(t)
	s.running = []int{60001}
	old := listProcesses
	t.Cleanup(func() { listProcesses = old })
	listProcesses = func(context.Context) ([]proc.Process, error) {
		return []proc.Process{{PID: 60001, Args: codexMacAppServer}}, nil
	}
	if err := RestartCodexApp(context.Background(), "/Applications/ChatGPT.app"); err == nil {
		t.Error("an old restart caller was told that Codex restarted")
	}
	s.running = []int{60001}
	if ended, left, err := RestartCodexAppWhenIdle(context.Background(), 0); ended != 0 || !left || err != nil {
		t.Errorf("uninstall: ended=%d pending=%v error=%v", ended, left, err)
	}
	if len(s.ended) != 0 {
		t.Fatalf("gateway restart/uninstall ended Codex app-servers %v", s.ended)
	}
}

func TestCodexLifecycleDaemonNeverRunsRestartCommand(t *testing.T) {
	pid := fakeDaemon(t)
	out := fakeCodex(t, 0)
	*pid = 50
	home := codexHome()
	writeDaemonPID(t, home, "50", time.Now().Add(-time.Hour))
	if err := RestartCodexDaemon(context.Background()); err == nil {
		t.Error("an old daemon restart caller was told that Codex restarted")
	}
	c := KeepCodexDaemonCurrent(context.Background(), home, time.Now())
	if !c.Behind || c.Restarted {
		t.Errorf("daemon must remain pending refresh: %+v", c)
	}
	if _, err := os.Stat(out + ".args"); !os.IsNotExist(err) {
		t.Fatalf("gateway executed the Codex daemon command: %v", err)
	}
}
