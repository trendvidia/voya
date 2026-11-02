package acceptance

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"slices"
	"testing"
	"time"

	voya "github.com/trendvidia/voya/client"
)

// --- #7: what the shutdown grace has to cover --------------------------------

// samples is how many restarts each condition is measured over.
//
// Enough that p99 has two samples above it rather than being the worst one,
// which is what stops a single descheduled goroutine deciding the result.
const samples = 200

// loadServices are registered and called throughout the busy half of the
// measurement, so the eviction path competes with proxying rather than running
// on an idle daemon.
var loadServices = []string{"one", "two", "three", "four"}

// measureOnce registers a service, tears its registration down, and returns how
// long voya took to stop handing that service out.
//
// The registry is the thing polled, and it is the right end of the interval
// without being quite all of it: the proxy reads the registry once per request
// (internal/proxy, Lookup then ServeHTTP), so a request that got past that read
// a moment earlier is still on its way to the old port after the registry has
// flipped. That residual is one handler dispatch and cannot be observed from
// out here — what covers it is the two orders of magnitude between these
// numbers and the grace, not this measurement.
func measureOnce(t *testing.T, v *voyad, name string, seq int) time.Duration {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	stop, err := voya.Join(context.Background(), name, ln.Addr(),
		voya.WithRegistrationPort(v.registrationPort),
		voya.WithoutDialer(),
		voya.WithBackoff(10*time.Millisecond, 200*time.Millisecond),
		voya.WithLogf(func(string, ...any) {}),
		// The grace off: this measures the interval the grace exists to cover,
		// and leaving it on would add it to every one of the samples.
		voya.WithShutdownGrace(0),
		// Each trial is a distinct process as far as the registry is concerned,
		// so a restart is an eviction rather than a re-registration.
		voya.WithPID(int32(seq+1)),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	waitFor(t, name+" to register", func() bool {
		_, live := v.Registry().Lookup(name)
		return live
	})

	// stop runs concurrently with the poll: the eviction can land before stop
	// returns, and a poll that only started afterwards would be measuring from
	// the wrong end of the interval.
	start := time.Now()
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()

	var elapsed time.Duration
	deadline := start.Add(20 * time.Second)
	for {
		if _, live := v.Registry().Lookup(name); !live {
			elapsed = time.Since(start)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was still routable 20s after its stream closed", name)
		}
		// Yielding rather than spinning flat out. The eviction being waited for
		// needs the registry's write lock, and on a busy machine it needs a
		// core — a poll that refuses to give either up measures its own
		// starvation.
		runtime.Gosched()
	}

	// Joined after the reading is taken, so the wait costs the measurement
	// nothing — but joined. Returning here with stop still in flight would
	// leave its registration client, and the pooled connection to voya behind
	// it, alive for the rest of the test; four hundred of those is the machine
	// pressure this test then blames the tail on.
	<-stopped
	return elapsed
}

// distribution is the shape of a set of samples. No mean: a grace period is
// chosen against the tail, and an average hides exactly the samples that
// decide whether a restart drops a request.
type distribution struct {
	n                  int
	p50, p90, p99, max time.Duration
}

// summarize takes trials, not `samples`: that name is the package-level count
// above, and shadowing an int with a slice thirty lines from its only other
// use is a reading trap for no gain.
func summarize(trials []time.Duration) distribution {
	sorted := slices.Clone(trials)
	slices.Sort(sorted)
	at := func(q float64) time.Duration { return sorted[int(q*float64(len(sorted)-1))] }
	return distribution{
		n:   len(sorted),
		p50: at(0.50), p90: at(0.90), p99: at(0.99),
		max: sorted[len(sorted)-1],
	}
}

// spread is p99 over p50: how much wider the tail is than the middle. Near 1
// means every sample paid the same delay, which is what a wait on the eviction
// path looks like; large means some samples lost their core, which is what a
// busy machine looks like.
func (d distribution) spread() float64 {
	if d.p50 <= 0 {
		return 0
	}
	return float64(d.p99) / float64(d.p50)
}

func (d distribution) String() string {
	round := func(t time.Duration) time.Duration { return t.Round(time.Microsecond) }
	return fmt.Sprintf("n=%d  p50=%s  p90=%s  p99=%s  max=%s",
		d.n, round(d.p50), round(d.p90), round(d.p99), round(d.max))
}

// TestEvictionPropagationStaysInsideTheGrace is the measurement behind
// [voya.DefaultShutdownGrace], kept as a test so the number cannot quietly
// stop being true (#7).
//
// The quantity is eviction propagation: from a service closing its
// registration stream to voya no longer routing to it. The grace has to exceed
// it, because a service that closes its listener while voya is still routing
// there turns ordinary restarts into intermittent connection resets.
//
// # Which statistic is asserted, and why it differs by condition
//
// The median is asserted under both conditions and the tail under only one,
// because the two answer different questions and only one of them is about
// voya.
//
// A wait added to the eviction path — a timer, a lock held across a syscall —
// delays every sample by about the same amount, so it moves the median and
// *narrows* the spread. Something else running on the machine delays whichever
// samples happen to lose their core, so it leaves the median where it was and
// *widens* the spread. The shapes are opposite, which is what makes them
// separable without knowing anything about the machine.
//
// This is not hypothetical. On the self-hosted Linux runner, where the
// cross-compile and lint jobs run on the same host as this one, the loaded
// condition reported p50=5.2ms against p99=66.8ms — a thirteen-fold spread
// with the median still two orders of magnitude inside the eviction path's
// own cost. An earlier version of this test asserted p99 against the grace and
// failed there, reporting "something on the eviction path is waiting" about a
// machine that was merely busy. It was measuring its neighbours.
//
// So: the median carries the regression check, everywhere. The tail is
// asserted only with voya idle, which is the reproducible condition and the
// one [voya.DefaultShutdownGrace] was chosen against. Under load the tail is
// still measured and still logged — it is the evidence that the grace's
// adequacy depends on the machine, which ADR 004 now says outright — but it is
// not a gate, because what it grades is whatever else the machine is doing.
//
// "voya idle" is a statement about the harness, not the host, and the same
// separation applies once more at the host level (#82): when the 1-minute
// load average exceeds one runnable thread per core, the tail gates are
// skipped — visibly, with the load in the skip message — because at that
// point one descheduled sample in two hundred is the expected output of the
// scheduler, not of voya. #82's spurious trip had max=182.6ms at load 38 with
// the median at 644µs; the numbers behind the gates were taken at loads
// 2.69–3.10 on the same 16-core machine, and normalising by core count is
// what puts both runs on the correct side of the same threshold.
func TestEvictionPropagationStaysInsideTheGrace(t *testing.T) {
	v := startVoya(t)

	measure := func(t *testing.T, what string, gateTail bool) {
		t.Helper()
		// Read before sampling, so the skip decision reflects the machine this
		// run competed with rather than the load the run itself generated.
		load, loadKnown := load1()
		trials := make([]time.Duration, 0, samples)
		for i := range samples {
			trials = append(trials, measureOnce(t, v, "measured", i))
		}
		d := summarize(trials)
		t.Logf("%s: %s", what, d)

		// Everywhere: half of all restarts must propagate well inside the
		// grace. Nothing a busy machine does moves this without moving the
		// tail with it by much more, and nothing added to the eviction path
		// leaves it alone.
		if d.p50 >= voya.DefaultShutdownGrace {
			t.Errorf("%s: median propagation is %s, at or past the %s grace — half of all restarts "+
				"drop the requests voya routed after the stream closed. A busy machine widens the "+
				"spread (here p99/p50 = %.1f) and leaves the median; a wait on the eviction path is "+
				"what moves the median.", what, d.p50, voya.DefaultShutdownGrace, d.spread())
		}
		if !gateTail {
			return
		}
		// The tail gates bet on a quiet host, and past this point the bet was
		// known lost before it was placed: a 1-minute load average above one
		// runnable thread per core means the run queue is longer than the
		// machine is wide, and the tail grades whatever else is running (#82:
		// max=182.6ms at load 38, median still 78× inside the grace). The
		// threshold is normalised by core count so it states the same thing on
		// a 4-core runner and a 16-core laptop. The median gated above,
		// unconditionally: a failure there stands, and this skip never reaches
		// it. The load is host-wide but NumCPU is process-schedulable CPUs, so
		// under an affinity mask or a cpuset the ratio inflates and these gates
		// skip early; under a CPU quota, which NumCPU does not see at all, it
		// under-states contention and they stay armed instead. Both are
		// accepted assumptions, not bugs: ADR 004's 2026-08-14 (#113)
		// amendment.
		if perCore := load / float64(runtime.NumCPU()); loadKnown && perCore > 1 {
			if t.Failed() {
				// The median already failed, so the tails would only grade the
				// neighbours — but say which gates were dropped and why, since
				// what ADR 004 records about this skip is that a gate not run
				// is visible rather than silent, and t.Skipf cannot say it here
				// without reporting a failed test as skipped.
				t.Logf("%s: not gating p99 or max either: the median failed above and the load is "+
					"%.2f per core, past 1.0 (#82)", what, perCore)
				return
			}
			t.Skipf("%s: skipping the p99 and max gates: 1-minute load average %.2f across %d cores "+
				"is %.2f per core, past 1.0 — the run queue is longer than the machine is wide, so "+
				"the tail measures the machine's other work, not voya (#82). The median gated as always.",
				what, load, runtime.NumCPU(), perCore)
		}
		if d.p99 >= voya.DefaultShutdownGrace {
			t.Errorf("%s: p99 propagation is %s, at or past the %s grace — one restart in a hundred "+
				"drops the requests voya routed after the stream closed", what, d.p99, voya.DefaultShutdownGrace)
		}
		if limit := 2 * voya.DefaultShutdownGrace; d.max >= limit {
			t.Errorf("%s: slowest propagation of %d was %s, past %s, with nothing else running "+
				"against voya", what, d.n, d.max, limit)
		}
	}

	t.Run("idle", func(t *testing.T) { measure(t, "voya idle", true) })

	t.Run("proxying", func(t *testing.T) {
		for _, name := range loadServices {
			startService(t, v, name, name+"/load")
		}
		// Background traffic, so nowhere to record outcomes: the caller these
		// stand in for is the one being measured elsewhere, and errors here
		// are the load generator racing the measurement's own restarts rather
		// than a claim about routing. [loadUntilStopped] takes a nil
		// [outcomes] for exactly that.
		stopLoad := loadUntilStopped(t, v, 2*len(loadServices), nil, loadServices...)
		defer stopLoad()
		measure(t, "voya proxying", false)
	})
}
