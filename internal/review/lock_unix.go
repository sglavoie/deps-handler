//go:build unix

package review

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes the lock file at path for this process, so two runs do not
// share checkouts. It fails at once when another process holds it; the
// lock goes when release is called or the process exits.
func Lock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another deps-merger run is reviewing updates (it holds %s)", path)
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
