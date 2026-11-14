package report

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedib0t/go-pretty/v6/text"
)

func TestStatusClockOutputAndClose(t *testing.T) {
	var stderr, stdout bytes.Buffer
	now := time.Unix(0, 0)
	s := newStatus(context.Background(), &stderr, true, func() time.Time { return now }, func() int { return 100 }, nil, func() {})
	defer s.Close()
	s.Set("reviewing acme/billing#58 with claude · 2/5 repos")
	now = now.Add(42 * time.Second)
	wantErr := errors.New("output failed")
	if err := s.WithOutput(func() error { return wantErr }); err != wantErr {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "⠋ reviewing acme/billing#58 with claude · 2/5 repos · 0:42") {
		t.Fatal(stderr.String())
	}
	before := stderr.Len()
	err := s.WithOutput(func() error {
		if got := stderr.String()[before:]; got != "\r\x1b[K" {
			t.Fatalf("not cleared before output: %q", got)
		}
		return Table(&stdout, []string{"PR", "OUTCOME"}, [][]string{{"acme/billing#58", "approved"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := Table(&plain, []string{"PR", "OUTCOME"}, [][]string{{"acme/billing#58", "approved"}}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != plain.String() {
		t.Fatal("status changed stdout")
	}
	if !strings.Contains(stderr.String()[before:], "⠙ reviewing") {
		t.Fatal("did not redraw")
	}
	s.Progress("Waiting for CI\nwith\tcontrol\x1b\r")
	if !strings.Contains(stderr.String(), "\r\x1b[KWaiting for CI with control\n") {
		t.Fatalf("unsafe progress: %q", stderr.String())
	}
	s.Close()
	end := stderr.String()
	s.Set("late update")
	_ = s.WithOutput(func() error { return nil })
	s.Close()
	if stderr.String() != end || !strings.HasSuffix(end, "\r\x1b[K") {
		t.Fatalf("not stopped: %q", stderr.String())
	}
}

func TestStatusNeverAnimatesNonterminalEvenWhenColorForced(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	var b bytes.Buffer
	s := NewStatus(context.Background(), &b)
	s.Set("working")
	_ = s.WithOutput(func() error { return nil })
	s.Progress("Running claude")
	s.Close()
	if b.String() != "Running claude\n" {
		t.Fatalf("redirected stderr: %q", b.String())
	}
}

func TestStatusFitsOneLineAndKeepsElapsed(t *testing.T) {
	for _, width := range []int{1, 12, 40} {
		var b bytes.Buffer
		s := newStatus(context.Background(), &b, true, func() time.Time { return time.Unix(0, 0) }, func() int { return width }, nil, func() {})
		s.Set(strings.Repeat("界", 80))
		_ = s.WithOutput(func() error { return nil })
		line := strings.TrimPrefix(b.String(), "\r\x1b[K")
		if text.StringWidth(line) >= width || strings.Contains(line, "\n") {
			t.Fatalf("width %d: %q", width, line)
		}
		if width >= 12 && !strings.HasSuffix(line, "0:00") {
			t.Fatalf("elapsed hidden: %q", line)
		}
		s.Close()
	}
}

type statusBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	wrote chan struct{}
}

func (b *statusBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.Buffer.Write(p)
	select {
	case b.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func TestStatusTickAndCancellationJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &statusBuffer{wrote: make(chan struct{}, 1)}
	ticks := make(chan time.Time)
	stopped := false
	s := newStatus(ctx, b, true, func() time.Time { return time.Unix(0, 0) }, func() int { return 80 }, ticks, func() { stopped = true })
	defer s.Close()
	s.Set("waiting for CI")
	ticks <- time.Unix(0, 0)
	select {
	case <-b.wrote:
	case <-time.After(time.Second):
		t.Fatal("ticker did not draw")
	}
	cancel()
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop animation")
	}
	if !stopped || !strings.HasSuffix(b.Buffer.String(), "\r\x1b[K") {
		t.Fatal("cancel did not clear and stop ticker")
	}
}
