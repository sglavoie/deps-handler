//go:build !unix

package review

import "errors"

// Lock refuses: without flock and process groups, a review could share
// checkouts with another run and leave an agent's processes behind.
func Lock(string) (func(), error) {
	return nil, errors.New("agent review is only supported on Linux and macOS")
}

// waitLock does nothing where flock is not available: a write to the
// verdict cache can then lose another process's concurrent write.
func waitLock(string) (func(), error) { return func() {}, nil }
