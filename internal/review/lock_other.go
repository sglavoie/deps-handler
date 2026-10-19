//go:build !unix

package review

// Lock does nothing where flock is not available.
func Lock(string) (func(), error) { return func() {}, nil }
