// Package lsof asks the host what process holds a port, for the two places
// that name a holder to a developer: `voya run`'s startup failure when a port
// is taken (ADR 001), and the diagnostic commands when something accepts on
// the proxy port that is not voya. Best effort by construction: lsof
// is not everywhere, it is not on Windows, and it reports nothing for a socket
// owned by another user. A blank answer costs the message one line; guessing
// would cost it its credibility.
package lsof

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Network is the transport a port is asked about, spelled as lsof's -i
// selector wants it — and it matters: asking about TCP finds nothing at all
// on the UDP port DNS binds.
type Network string

const (
	TCP Network = "TCP"
	UDP Network = "UDP"
)

// Holder names the process on port, "command (pid N)" when both are known and
// whichever one is when not, or "" when nothing could be learned — lsof
// absent, refused, or silent.
func Holder(ctx context.Context, network Network, port uint32) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	args := []string{"-nP", "-i" + string(network) + ":" + strconv.FormatUint(uint64(port), 10)}
	if network == TCP {
		// Keeps this to the listener rather than every connection to it,
		// which on a busy port is the difference between one line and
		// hundreds. UDP has no such state to select on.
		args = append(args, "-sTCP:LISTEN")
	}
	args = append(args, "-F", "cpn")

	//nolint:gosec // G204: no shell is involved, and the only interpolated
	// value is a uint32 rendered as decimal — it cannot carry anything but
	// digits.
	cmd := exec.CommandContext(ctx, "lsof", args...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return Summarize(strings.NewReader(string(out)))
}

// Summarize reads lsof's field output (-F cpn) into one line: the first
// process it names, as [Holder] words it.
func Summarize(r io.Reader) string {
	data, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	var pid, command string
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid = line[1:]
		case 'c':
			command = line[1:]
		}
		if pid != "" && command != "" {
			return fmt.Sprintf("%s (pid %s)", command, pid)
		}
	}
	if command != "" {
		return command
	}
	if pid != "" {
		return "pid " + pid
	}
	return ""
}
