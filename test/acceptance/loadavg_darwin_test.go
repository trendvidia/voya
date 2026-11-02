package acceptance

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// load1 returns the host's 1-minute load average — the first number uptime(1)
// prints — read from sysctl vm.loadavg: a struct loadavg of three fixed-point
// uint32 samples, alignment padding, and the scale as a C long, in native
// byte order. syscall.Sysctl hands the struct back as a string with one
// trailing NUL trimmed, so the scale is reassembled byte-by-byte instead of
// sliced at its full width. TestLoad1AgreesWithTheKernel is what keeps the
// offsets honest.
func load1() (float64, bool) {
	raw, err := syscall.Sysctl("vm.loadavg")
	if err != nil || len(raw) < 20 {
		return 0, false
	}
	sample := uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24
	var scale uint64
	for i := 0; i < 8 && 16+i < len(raw); i++ {
		scale |= uint64(raw[16+i]) << (8 * i)
	}
	if scale == 0 {
		return 0, false
	}
	return float64(sample) / float64(scale), true
}

// TestLoad1AgreesWithTheKernel checks the struct decode above against what
// sysctl(8) prints for the same value — a wrong offset or width reads padding
// and lands orders of magnitude away, not within rounding. The kernel updates
// the average between reads, so load1 is bracketed by a CLI read on each side
// rather than compared to one, ±0.01 for the CLI's two-decimal rounding.
//
// A floor on what the bracket can prove (#113): it discriminates only while
// the 1-, 5- and 15-minute averages differ, so on a long-idle host where all
// three have converged a decode that read load5's or load15's bytes — a valid
// sample at the wrong offset — passes anyway. A green run on an idle host is
// therefore not evidence the field is right; the decode was mutation-proved
// on a loaded host during PR #125's review, by pointing the sample at
// load5's offset. (PR #111's review proved the Linux parse, not this one.)
func TestLoad1AgreesWithTheKernel(t *testing.T) {
	fromCLI := func() float64 {
		out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			t.Fatalf("sysctl -n vm.loadavg: %v", err)
		}
		fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
		if len(fields) < 1 {
			t.Fatalf("sysctl -n vm.loadavg printed %q, expected { load1 load5 load15 }", out)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			t.Fatalf("sysctl -n vm.loadavg printed %q: %v", out, err)
		}
		return v
	}

	before := fromCLI()
	got, ok := load1()
	after := fromCLI()
	if !ok {
		t.Fatalf("load1 could not read vm.loadavg on the platform whose kernel provides it")
	}
	lo, hi := min(before, after)-0.01, max(before, after)+0.01
	if got < lo || got > hi {
		t.Errorf("load1 read %v; sysctl read %v then %v — outside [%v, %v], so the struct decode is off", got, before, after, lo, hi)
	}
}
