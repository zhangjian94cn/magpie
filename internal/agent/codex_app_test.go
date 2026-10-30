package agent

import (
	"context"
	"strings"
	"testing"
)

// Old desktop callers get an explicit refusal and never use generic app helpers.
func TestCodexAppReopen(t *testing.T) {
	oldQuit, oldOpen := appQuit, appOpen
	t.Cleanup(func() { appQuit, appOpen = oldQuit, oldOpen })
	appQuit = func(context.Context, string) error { t.Fatal("quit Codex"); return nil }
	appOpen = func(context.Context, string) error { t.Fatal("opened Codex"); return nil }
	for _, bundle := range []string{"", "/Applications/Codex.app", "/Applications/Other.app"} {
		err := ReopenCodexApp(context.Background(), bundle)
		if err == nil || !strings.Contains(err.Error(), "magpie did not restart it") {
			t.Fatalf("bundle %q: %v", bundle, err)
		}
	}
	if err := (&Agent{ID: "codex", Name: "Codex"}).RestartApp(context.Background()); err == nil || !strings.Contains(err.Error(), "magpie did not restart it") {
		t.Fatalf("generic restart: %v", err)
	}
}

// Windows' former recovery path must not close the desktop app or its tree.
func TestCodexWindowsRecoveryOnlyExplainsRefresh(t *testing.T) {
	restartSandbox(t)
	winClose = func([]int) error { t.Fatal("closed Codex windows"); return nil }
	winEnd = func(int) error { t.Fatal("ended Codex process tree"); return nil }
	winLaunch = func(winProc) error { t.Fatal("relaunched Codex"); return nil }
	for _, windows := range []bool{false, true} {
		restartOnWindows = windows
		for _, call := range []func(context.Context) error{codexAppReopen, RestartSwitchedCodexApp} {
			err := call(context.Background())
			if err == nil || !strings.Contains(err.Error(), "magpie did not restart it") {
				t.Fatalf("Windows=%v: %v", windows, err)
			}
		}
	}
}
