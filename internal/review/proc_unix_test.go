//go:build unix

package review

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCancelKillsAgentChildren(t *testing.T) {
	dir := t.TempDir()
	cmd, _ := fakeAgent(t, `sleep 30 & echo $! > "`+dir+`/child"; wait`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (Claude{Command: cmd}).Run(ctx, t.TempDir(), "p")
		done <- err
	}()
	// Cancel once the agent has started its child.
	pid := 0
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the agent never started its child")
		}
		data, _ := os.ReadFile(dir + "/child")
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected the cancelled agent to fail")
	}
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("the agent's child outlived it")
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "review.lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); err == nil || !strings.Contains(err.Error(), "another deps-merger run") {
		t.Fatalf("second lock: %v", err)
	}
	release()
	release, err = Lock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release()
}
