package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sglavoie/deps-merger/pkg/github"
)

type cancellingClient struct {
	*fakeClient
	cancel context.CancelFunc
}

func (c cancellingClient) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	repo, err := c.fakeClient.FetchRepo(ctx, owner, name)
	c.cancel()
	return repo, err
}

func TestRunHonorsCommandCancellation(t *testing.T) {
	for _, duringFetch := range []bool{false, true} {
		t.Run(map[bool]string{false: "before run", true: "during fetch"}[duringFetch], func(t *testing.T) {
			fc, cfg := runFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if duringFetch {
				newClient = func() (github.Client, error) { return cancellingClient{fc, cancel}, nil }
			} else {
				cancel()
			}
			root := newRoot()
			root.SetArgs([]string{"run", "--apply", "-c", cfg, "-r", "busy"})
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			if err := root.ExecuteContext(ctx); !errors.Is(err, errInterrupted) {
				t.Errorf("error = %v; want interrupted", err)
			}
			if len(fc.merges) != 0 {
				t.Errorf("merged %v after command cancellation", fc.merges)
			}
		})
	}
}

// failOutput fails just once, so a later successful write cannot hide it.
type failOutput struct {
	remaining int
	err       error
	failed    bool
}

func (w *failOutput) Write(p []byte) (int, error) {
	if w.failed || len(p) <= w.remaining {
		w.remaining -= len(p)
		return len(p), nil
	}
	w.failed = true
	return max(0, w.remaining), w.err
}

func TestRunReportsOutputFailure(t *testing.T) {
	for _, format := range []string{"text", "--markdown", "--json"} {
		t.Run(format, func(t *testing.T) {
			_, cfg := runFixture(t)
			args := []string{"run", "-c", cfg}
			if format != "text" {
				args = append(args, format)
			}
			_, output, _ := run(args...)
			for _, after := range []int{0, len(output) / 2, len(output) - 1} {
				want := errors.New("output full")
				w := &failOutput{remaining: after, err: want}
				var stderr bytes.Buffer
				if code := execute(args, w, &stderr); code != 1 || !bytes.Contains(stderr.Bytes(), []byte(want.Error())) {
					t.Errorf("after %d bytes: code %d, stderr %q; want output failure", after, code, stderr.String())
				}
			}
		})
	}
}
