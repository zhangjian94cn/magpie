package provider

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// The newer account-switch API must obey the same boundary as the older APIs.
func TestRestartSwitchedCodexAppLeavesPendingRefresh(t *testing.T) {
	s := stubCodexApp(t)
	t.Cleanup(DismissCodexApp)
	ctx := context.Background()
	for _, running := range [][]int{nil, {7, 8}} {
		s.running = slices.Clone(running)
		codexDaemonMu.Lock()
		codexAppLeft = leftDaemon{pid: 7, user: "spent@example.test"}
		codexDaemonMu.Unlock()
		if err := RestartSwitchedCodexApp(ctx); !errors.Is(err, errCodexRefreshRequired) {
			t.Fatalf("running %v: %v", running, err)
		}
		codexDaemonMu.Lock()
		left := codexAppLeft
		codexDaemonMu.Unlock()
		if len(s.ended) != 0 || !slices.Equal(s.running, running) || left.user != "spent@example.test" {
			t.Fatalf("running=%v ended=%v pending=%+v", s.running, s.ended, left)
		}
	}
}

// Neither a spent account nor a long silence overrides an unfinished turn.
func TestCodexSpentSwitchNeverRestartsPastQuietTools(t *testing.T) {
	signIn(t)
	s := stubCodexApp(t)
	s.running, s.turnOpen = []int{7}, true
	idle := func() (int, time.Time) { return 0, time.Now().Add(-time.Hour) }
	codexAppStep(context.Background(), time.Now(), idle)
	codexAppSpentSwitch()
	for range 5 {
		codexAppStep(context.Background(), time.Now(), idle)
	}
	if len(s.ended) != 0 || !CodexAppStuck() {
		t.Fatalf("ended=%v pending=%v", s.ended, CodexAppStuck())
	}
}
