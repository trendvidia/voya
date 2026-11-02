package diag

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is Windows' connection-refused errno, WSAECONNREFUSED
// (10061), named by its number because golang.org/x/sys/windows cannot be
// reached from a file that also builds for unix.
//
// It has to be matched separately because [syscall.ECONNREFUSED] on Windows
// is not what a socket returns there: it is a synthetic value built on
// APPLICATION_ERROR (GOROOT's zerrors_windows.go), and Errno.Is maps only the
// four oserror sentinels, so errors.Is against it is false for every real
// refused dial on that OS. No unix errno reaches five digits, so the number
// claims nothing on the other targets `make cross` builds.
const wsaeconnrefused = syscall.Errno(10061)

// ConnectionRefused reports whether err is a dial nothing accepted, on every
// OS voya builds for. The plain errors.Is against [syscall.ECONNREFUSED] is
// the whole answer on unix and none of it on Windows, and the difference
// matters here: a free proxy port that reads as refused is `free`, verdict
// ok, while one that does not is `held-by-other`, verdict problem, telling
// the developer to stop a process that does not exist.
func ConnectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == wsaeconnrefused
}
