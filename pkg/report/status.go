package report

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/term"
)

// Status is a single transient stderr line. Call Close before final output and
// defer it for error paths. WithOutput coordinates intermediate output without
// wrapping its writer, preserving terminal styling and width detection.
type Status struct {
	mu                     sync.Mutex
	w                      io.Writer
	enabled, closed, drawn bool
	message                string
	started                time.Time
	now                    func() time.Time
	width                  func() int
	frame                  int
	stop, done             chan struct{}
}

// NewStatus animates only when w is an actual terminal, regardless of color
// overrides. Cancellation clears the line even while work is shutting down.
func NewStatus(ctx context.Context, w io.Writer) *Status {
	f, ok := w.(*os.File)
	enabled := ok && term.IsTerminal(int(f.Fd()))
	width := func() int {
		if enabled {
			if n, _, err := term.GetSize(int(f.Fd())); err == nil && n > 0 {
				return n
			}
		}
		return 80
	}
	var ticks <-chan time.Time
	stopTicker := func() {}
	if enabled {
		ticker := time.NewTicker(200 * time.Millisecond)
		ticks, stopTicker = ticker.C, ticker.Stop
	}
	return newStatus(ctx, w, enabled, time.Now, width, ticks, stopTicker)
}

func newStatus(ctx context.Context, w io.Writer, enabled bool, now func() time.Time, width func() int, ticks <-chan time.Time, stopTicker func()) *Status {
	s := &Status{w: w, enabled: enabled, now: now, started: now(), width: width, stop: make(chan struct{}), done: make(chan struct{})}
	if !enabled {
		stopTicker()
		close(s.done)
		return s
	}
	go func() {
		defer close(s.done)
		defer stopTicker()
		for {
			select {
			case <-ctx.Done():
				s.finish()
				return
			case <-s.stop:
				return
			case <-ticks:
				s.mu.Lock()
				s.draw()
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// Set changes the activity shown on the next refresh. Concurrent calls are safe.
func (s *Status) Set(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = Plain(message)
}

// Progress preserves a permanent progress message and updates the transient line.
func (s *Status) Progress(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	s.message = Plain(message)
	fmt.Fprintln(s.w, s.message)
	s.draw()
}

// WithOutput clears the status around a complete output operation. The callback
// must not call Status methods. Its error is returned unchanged.
func (s *Status) WithOutput(write func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	defer s.draw()
	return write()
}

func (s *Status) clear() {
	if s.drawn {
		fmt.Fprint(s.w, "\r\x1b[K")
		s.drawn = false
	}
}

func (s *Status) draw() {
	if !s.enabled || s.closed || s.message == "" {
		return
	}
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	seconds := max(0, int(s.now().Sub(s.started).Seconds()))
	prefix := fmt.Sprintf("%c ", frames[s.frame%len(frames)])
	suffix := fmt.Sprintf(" · %d:%02d", seconds/60, seconds%60)
	width := max(0, s.width()-1)
	message := text.Trim(s.message, max(0, width-text.StringWidth(prefix+suffix)))
	line := prefix + message + suffix
	s.frame++
	// Leave a column unused so even a full-width line cannot wrap or scroll.
	line = text.Trim(line, width)
	fmt.Fprint(s.w, "\r\x1b[K", line)
	s.drawn = true
}

func (s *Status) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.clear()
		close(s.stop)
	}
}

// Close clears the line and joins the animation goroutine. It is idempotent.
func (s *Status) Close() {
	s.finish()
	<-s.done
}
