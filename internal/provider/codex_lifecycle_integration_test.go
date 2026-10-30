package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// A real isolated backend owns two real tool processes. Each tool waits for
// input, so the quiet period has no completion deadline for a loaded runner.
func TestCodexLifecycleProcessHelper(t *testing.T) {
	mode := os.Getenv("MAGPIE_LIFECYCLE_PROCESS")
	if mode == "" {
		return
	}
	if mode == "tool" {
		fmt.Println("ready " + os.Getenv("MAGPIE_LIFECYCLE_TASK"))
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			os.Exit(2)
		}
		fmt.Println("complete " + os.Getenv("MAGPIE_LIFECYCLE_TASK"))
		os.Exit(0)
	}
	var tools []*os.Process
	var inputs []io.WriteCloser
	for _, name := range []string{"alpha", "beta"} {
		cmd := proc.Command(os.Args[0], "-test.run=^TestCodexLifecycleProcessHelper$")
		cmd.Env = append(os.Environ(), "MAGPIE_LIFECYCLE_PROCESS=tool", "MAGPIE_LIFECYCLE_TASK="+name)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil || cmd.Start() != nil {
			os.Exit(3)
		}
		tools, inputs = append(tools, cmd.Process), append(inputs, in)
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		for _, p := range tools {
			_ = p.Kill()
		}
		os.Exit(4)
	}
	for _, in := range inputs {
		_, _ = io.WriteString(in, "finish\n")
		_ = in.Close()
	}
	for _, p := range tools {
		st, err := p.Wait()
		if err != nil || !st.Success() {
			os.Exit(5)
		}
	}
	fmt.Printf("backend complete %d\n", os.Getpid())
	os.Exit(0)
}

func TestCodexLifecycleTwoParallelLongTools(t *testing.T) {
	signIn(t)
	stubCodexApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexLifecycleProcessHelper$")
	cmd.Env = append(os.Environ(), "MAGPIE_LIFECYCLE_PROCESS=backend")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	waited := false
	defer func() {
		if !waited {
			_ = in.Close()
			_ = cmd.Wait()
		}
	}()
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		scan := bufio.NewScanner(out)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()
	ready := map[string]bool{}
	for len(ready) < 2 {
		select {
		case line, ok := <-lines:
			if !ok || !strings.HasPrefix(line, "ready ") {
				t.Fatalf("tools failed to start: %q", line)
			}
			ready[strings.TrimPrefix(line, "ready ")] = true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var routeMu sync.Mutex
	route := "catalog-v1"
	CodexAppRoute = func() (string, bool, bool) {
		routeMu.Lock()
		defer routeMu.Unlock()
		return route, false, true
	}
	codexAppServersOn = func(context.Context, string) ([]int, error) { return []int{pid}, nil }
	var ends atomic.Int32
	endCodexAppServer = func(int) error { ends.Add(1); return nil }
	// The previous model reply is older than the old ten-second threshold.
	idle := func() (int, time.Time) { return 0, time.Now().Add(-time.Hour) }
	codexAppStep(ctx, time.Now(), idle)
	watch, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); KeepCodexAppOnItsProvider(watch, idle) }()
	defer func() { stop(); <-done }()
	quiet := time.Now()
	select {
	case line := <-lines:
		t.Fatalf("a tool completed before release: %q", line)
	case <-time.After(11 * time.Second):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, change := range []string{"catalog-v2", "account-v2", "endpoint-v2", "", "connected-again"} {
		routeMu.Lock()
		route = change
		routeMu.Unlock()
		codexAppAccountSwitched()
		codexAppStep(ctx, time.Now(), idle)
		if !CodexAppStuck() || ends.Load() != 0 {
			t.Fatalf("%s: pending=%v terminations=%d", change, CodexAppStuck(), ends.Load())
		}
	}
	pids, err := codexAppServersOn(ctx, codexHome())
	if err != nil || !slices.Equal(pids, []int{pid}) {
		t.Fatalf("backend changed: %v %v", pids, err)
	}
	_, _ = io.WriteString(in, "finish\n")
	var completed []string
	for line := range lines {
		completed = append(completed, line)
	}
	err = cmd.Wait()
	waited = true
	if err != nil || !slices.Contains(completed, "complete alpha") ||
		!slices.Contains(completed, "complete beta") ||
		!slices.Contains(completed, fmt.Sprintf("backend complete %d", pid)) || ends.Load() != 0 {
		t.Fatalf("backend=%d ends=%d output=%v error=%v", pid, ends.Load(), completed, err)
	}
	t.Logf("two parallel tools completed; quiet=%s backend_pid=%d unchanged; termination_calls=%d",
		time.Since(quiet).Round(time.Millisecond), pid, ends.Load())
}
