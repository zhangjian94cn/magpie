//go:build !windows

package gui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
)

// Old API clients receive pending refresh with no daemon command executed.
func TestCodexDaemonRestartRoute(t *testing.T) {
	sandboxHome(t)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("CODEX_HOME", "/somewhere/else")
	rec := filepath.Join(bin, "ran")
	script := "#!/bin/sh\necho \"$@|$CODEX_HOME\" > '" + rec + "'\n[ \"$FAIL\" = 1 ] && { echo 'Error: no daemon' >&2; exit 1; }\nexit 0\n"
	testenv.Program(t, filepath.Join(bin, "codex"), script)
	mux := Handler(nil, nil)
	post := func(action string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/codex/daemon/"+action, strings.NewReader("{}")))
		return w
	}
	for _, endpoint := range []string{"/api/codex/daemon/restart", "/api/codex/daemon/restart-app", "/api/codex/app/reopen", "/api/agents/restart/codex"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", endpoint, strings.NewReader(`{"bundle":"/Applications/Codex.app"}`)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "magpie did not restart it") {
			t.Fatalf("%s: %d %s", endpoint, w.Code, w.Body)
		}
	}
	if _, err := os.Stat(rec); !os.IsNotExist(err) {
		t.Fatalf("ran Codex: %v", err)
	}
	if w := post("dismiss"); w.Code != 200 {
		t.Fatalf("dismiss: %d %s", w.Code, w.Body)
	}
	if w := post("dismiss-app"); w.Code != 200 {
		t.Fatalf("dismiss-app: %d %s", w.Code, w.Body)
	}
	if w := post("nope"); w.Code != 404 {
		t.Fatalf("unknown action: %d", w.Code)
	}
}
