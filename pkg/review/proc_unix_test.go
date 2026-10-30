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

func TestAgentSucceedsDespiteChildHoldingOutput(t *testing.T) {
	defer func(d time.Duration) { waitDelay = d }(waitDelay)
	waitDelay = 100 * time.Millisecond
	dir := t.TempDir()
	// The child keeps stdout open after the agent exits 0.
	cmd, _ := fakeAgent(t, `sleep 30 & echo $! > "`+dir+`/child"
echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{"decision":"approve","summary":"ok","breaking_changes":[],"concerns":[]}}'
exit 0`)
	a, err := (Claude{Command: cmd}).Run(context.Background(), t.TempDir(), "p")
	if err != nil || a.Decision != "approve" {
		t.Fatalf("answer %+v, %v; a clean exit must count as success", a, err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(read(t, dir+"/child")))
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("the agent's child outlived its success")
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "review.lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); err == nil || !strings.Contains(err.Error(), "another deps-handler run") {
		t.Fatalf("second lock: %v", err)
	}
	release()
	release, err = Lock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release()
}

func TestCacheRecoveryWaitsForTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verdicts.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Another process is writing the cache: what OpenCache read was cut
	// short, and the file is whole by the time the lock is released.
	unlock, err := waitLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *Cache, 1)
	go func() {
		c, err := OpenCache(path)
		if err != nil {
			t.Error(err)
		}
		done <- c
	}()
	time.Sleep(100 * time.Millisecond)
	whole := `{"o/r#1 a@1": {"verdict": {"approved": true}, "reviewed_at": "` + time.Now().UTC().Format(time.RFC3339) + `"}}`
	if err := os.WriteFile(path, []byte(whole), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock()
	c := <-done
	if c == nil {
		t.FailNow()
	}
	if aside, _ := filepath.Glob(path + ".corrupt-*"); len(aside) != 0 || c.TakeRecovered() != "" || len(c.Entries()) != 1 {
		t.Fatalf("aside %v, entries %+v; a file made whole under the lock must be read, not moved aside", aside, c.Entries())
	}
}
