package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
)

// Codex owns its app-server lifecycle. The shared app helpers below are
// still used by other desktop agents, never by Codex's refresh notice.

// appQuit and appOpen ask macOS to quit a bundle and to open it; vars so
// tests don't touch a real app.
var (
	appQuit = func(ctx context.Context, bundle string) error {
		out, err := proc.CommandContext(ctx, "osascript", "-e", `tell application "`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(bundle)+`" to quit`).CombinedOutput()
		return cmdErr(out, err)
	}
	appOpen = func(ctx context.Context, bundle string) error {
		out, err := proc.CommandContext(ctx, "open", bundle).CombinedOutput()
		return cmdErr(out, err)
	}
)

// appQuitWait is how long a quit app's processes have to end.
var appQuitWait = 30 * time.Second

func cmdErr(out []byte, err error) error {
	if err != nil && len(strings.TrimSpace(string(out))) > 0 {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return err
}

// ReopenCodexApp preserves the old API with an explicit pending-refresh error.
func ReopenCodexApp(ctx context.Context, bundle string) error {
	return provider.RestartCodexApp(ctx, bundle)
}

// RestartSwitchedCodexApp keeps legacy clients informed in their language.
func RestartSwitchedCodexApp(context.Context) error {
	return errors.New(noticeCodexBehind.say("command", provider.CodexDaemonRestart))
}

// codexWinApp is the Windows Codex app: the OpenAI.Codex Store package's
// ChatGPT.exe (26.1021 on the Windows box, under C:\Program Files\
// WindowsApps\OpenAI.Codex_…\app\), never the ChatGPT chat app's.
var codexWinApp = restartable{winExe: "ChatGPT.exe", win: func(image string) bool {
	return winBase(image, "ChatGPT.exe") && winHas(image, `\WindowsApps\OpenAI.Codex_`)
}}

// codexAppOpen is provider.CodexAppOpen: whether the Codex desktop app
// runs here, ChatGPT.app (com.openai.codex) on macOS, the Store app on
// Windows; not known elsewhere.
func codexAppOpen(context.Context) (open, known bool) {
	switch {
	case restartOnWindows:
		_, ok := findRestartable(codexWinApp)
		return ok, true
	case restartOnMac:
		_, ok := findRestartable(restartable{mac: desktopApps["codex"]})
		return ok, true
	}
	return false, false
}

// The former Windows recovery path also leaves lifecycle control to Codex.
func codexAppReopen(ctx context.Context) error {
	return RestartSwitchedCodexApp(ctx)
}
