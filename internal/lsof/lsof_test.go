package lsof

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestSummarize(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"command and pid", "p4242\ncnode\nf7\nn127.0.0.1:9777\n", "node (pid 4242)"},
		{"pid only", "p4242\n", "pid 4242"},
		{"nothing", "", ""},
		{"header only", "\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Summarize(strings.NewReader(tc.out)); got != tc.want {
				t.Errorf("Summarize(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// TestHolderNamesThisProcess binds a TCP port in this process and asks who
// holds it: the answer names this test binary's own pid. Where lsof is not
// installed the answer is "" by contract, and the test says so rather than
// failing on a host it cannot measure.
func TestHolderNamesThisProcess(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not on this host; Holder answers \"\" here by contract")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	got := Holder(t.Context(), TCP, port)
	if want := "(pid " + strconv.Itoa(os.Getpid()) + ")"; !strings.Contains(got, want) {
		t.Errorf("Holder(TCP, %d) = %q, want it to name this process %q", port, got, want)
	}
}
