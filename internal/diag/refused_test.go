package diag

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// TestConnectionRefusedOnEveryTarget sweeps what a refused dial looks like on
// each OS `make cross` builds, from whichever one is running the test: the
// unix errno, the Windows one, each as the net package hands it over —
// wrapped in *net.OpError over *os.SyscallError — and the errors that are not
// a refusal at all. The Windows row is the one that matters, because
// syscall.ECONNREFUSED matches itself on every OS and so cannot fail there;
// what a Windows socket actually returns is WSAECONNREFUSED, and only this
// row asks about it.
func TestConnectionRefusedOnEveryTarget(t *testing.T) {
	dialed := func(errno syscall.Errno) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the unix errno", syscall.ECONNREFUSED, true},
		{"the unix errno as net hands it over", dialed(syscall.ECONNREFUSED), true},
		{"the unix errno wrapped", fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED), true},
		{"WSAECONNREFUSED as net hands it over", dialed(wsaeconnrefused), true},
		{"WSAECONNREFUSED wrapped", fmt.Errorf("dial tcp: %w", wsaeconnrefused), true},
		{"another errno", dialed(syscall.EACCES), false},
		{"not a syscall error", errors.New("unexpected HTTP status 404"), false},
		{"nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConnectionRefused(tc.err); got != tc.want {
				t.Errorf("ConnectionRefused(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
