package acceptance

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// load1 returns the host's 1-minute load average — the first field of
// /proc/loadavg, the same number uptime(1) prints.
func load1() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// TestLoad1AgreesWithTheKernel pins two things the tail gates in grace_test.go
// depend on. That the guard's input exists here: every Linux this runs on has
// /proc/loadavg, so a not-known answer would mean the parse broke and the
// gates silently lost their guard. And that the field the parse picks is the
// 1-minute average rather than the 5- or 15-minute one beside it — reading
// those would over-skip after a spike and under-skip during one, in both cases
// without a sign that anything was wrong.
//
// The independent source is sysinfo(2), which carries the same three averages
// as fixed point scaled by 1<<16, from the kernel struct rather than procfs
// text. It disagrees with a wrong field by whatever the machine's load has
// done in the last quarter hour, which on a runner part-way through a build is
// more than the tolerance here. As on darwin the kernel moves the value
// between reads, so load1 is bracketed by a read on each side rather than
// compared to one, ±0.01 for /proc/loadavg's two decimals.
//
// A floor on what the bracket can prove (#113): it discriminates only while
// the 1-, 5- and 15-minute averages differ, so on a long-idle host where all
// three have converged — 0.00 three times — a parse that picked the wrong
// field passes anyway. A green run on an idle host is therefore not evidence
// the field is right; the parse was mutation-proved on a loaded host during
// PR #111's review.
func TestLoad1AgreesWithTheKernel(t *testing.T) {
	fromSysinfo := func() float64 {
		var si syscall.Sysinfo_t
		if err := syscall.Sysinfo(&si); err != nil {
			t.Fatalf("sysinfo: %v", err)
		}
		return float64(si.Loads[0]) / (1 << 16)
	}

	before := fromSysinfo()
	got, ok := load1()
	after := fromSysinfo()
	if !ok {
		t.Fatalf("load1 could not read /proc/loadavg on the platform that provides it")
	}
	lo, hi := min(before, after)-0.01, max(before, after)+0.01
	if got < lo || got > hi {
		t.Errorf("load1 read %v; sysinfo read %v then %v — outside [%v, %v], so the parse is reading the wrong field of /proc/loadavg", got, before, after, lo, hi)
	}
}
