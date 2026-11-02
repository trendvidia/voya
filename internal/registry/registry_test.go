package registry

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustRegister(t *testing.T, r *Registry, inst Instance) *Registration {
	t.Helper()
	h, err := r.Register(inst)
	if err != nil {
		t.Fatalf("Register(%+v): %v", inst, err)
	}
	return h
}

func TestRegisterAndLookup(t *testing.T) {
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	r := New(WithClock(func() time.Time { return at }))
	mustRegister(t, r, Instance{Service: "payments", Port: 51234, PID: 4242})

	up, ok := r.Lookup("payments")
	if !ok {
		t.Fatal("Lookup after Register reported not found")
	}
	if up.Port != 51234 || up.PID != 4242 || up.Service != "payments" {
		t.Errorf("Lookup = %+v, want port 51234 pid 4242", up)
	}
	if !up.Since.Equal(at) {
		t.Errorf("Since = %v, want %v", up.Since, at)
	}

	if _, ok := r.Lookup("billing"); ok {
		t.Error("Lookup of an unregistered name reported found")
	}
}

func TestLookupAuthority(t *testing.T) {
	r := New()
	mustRegister(t, r, Instance{Service: "payments", Port: 51234})

	found := []string{
		"payments.internal:9777",
		"payments.internal",
		"PAYMENTS.INTERNAL:9777", // hostnames are case-insensitive
		"payments.internal.",     // fully qualified, trailing root dot
		"payments.internal.:9777",
	}
	for _, authority := range found {
		t.Run(authority, func(t *testing.T) {
			up, ok := r.LookupAuthority(authority)
			if !ok {
				t.Fatalf("LookupAuthority(%q) reported not found", authority)
			}
			if up.Port != 51234 {
				t.Errorf("port = %d, want 51234", up.Port)
			}
		})
	}

	// Not voya's to route. Each must report false so the proxy can write
	// the diagnostic ADR 005 asks for rather than route somewhere wrong.
	notOurs := []string{
		"payments.local:9777",       // wrong suffix
		"payments",                  // no suffix at all
		"api.payments.internal",     // multi-label: nobody registered this
		".internal",                 // empty name
		"[::1]:9777",                // IPv6 literal
		"localhost:9777",            //
		"paymnets.internal:9777",    // the typo ADR 005 is written around
		"payments.internal:notaint", // non-numeric port is part of the host
	}
	for _, authority := range notOurs {
		t.Run(authority, func(t *testing.T) {
			if _, ok := r.LookupAuthority(authority); ok {
				t.Errorf("LookupAuthority(%q) routed somewhere", authority)
			}
		})
	}
}

// TestEvictAndReplace is ADR 002's decision: last writer wins, and the
// incumbent is told.
func TestEvictAndReplace(t *testing.T) {
	r := New()
	first := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 11})

	select {
	case <-first.Evicted():
		t.Fatal("a sole registration was reported evicted")
	default:
	}

	second := mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 22})

	select {
	case <-first.Evicted():
	default:
		t.Fatal("the deposed instance was not told it was evicted")
	}
	by, ok := first.EvictedBy()
	if !ok {
		t.Fatal("EvictedBy reported still live")
	}
	if by.Port != 1002 || by.PID != 22 {
		t.Errorf("EvictedBy = %+v, want port 1002 pid 22", by)
	}
	if _, ok := second.EvictedBy(); ok {
		t.Error("the incoming instance was reported evicted")
	}

	up, _ := r.Lookup("payments")
	if up.Port != 1002 {
		t.Errorf("Lookup = port %d, want the incoming 1002", up.Port)
	}

	evictions := r.Evictions()
	if len(evictions) != 1 {
		t.Fatalf("Evictions() = %d records, want 1", len(evictions))
	}
	if evictions[0].Evicted.Port != 1001 || evictions[0].Incoming.Port != 1002 {
		t.Errorf("eviction record = %+v", evictions[0])
	}
}

// TestClosedHandleDoesNotRemoveReplacement is the edge that makes eviction
// safe: the deposed instance's stream teardown races its replacement's
// registration, and the loser must not delete the winner.
func TestClosedHandleDoesNotRemoveReplacement(t *testing.T) {
	r := New()
	first := mustRegister(t, r, Instance{Service: "payments", Port: 1001})
	mustRegister(t, r, Instance{Service: "payments", Port: 1002})

	first.Close() // late teardown of the deposed stream

	up, ok := r.Lookup("payments")
	if !ok {
		t.Fatal("the replacement was removed by the evicted instance's Close")
	}
	if up.Port != 1002 {
		t.Errorf("Lookup = port %d, want 1002", up.Port)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	r := New()
	h := mustRegister(t, r, Instance{Service: "payments", Port: 1001})
	h.Close()
	h.Close()
	if _, ok := r.Lookup("payments"); ok {
		t.Error("Lookup found a closed registration")
	}
}

// TestNoWindowWhileReplacing covers two acceptance criteria at once.
//
// The name must never blink out of existence while a service restarts into
// its own live entry — a request landing in that window fails for a service
// that is up. And traffic must never move *backwards* to an older instance,
// which is the "silent round-robin between two builds" ADR 002 rules out:
// ports here increase with each registration, so an observed port that ever
// decreases is exactly that failure.
//
// The Close ordering is the real-world race — the deposed stream tears down
// after its replacement has already registered.
func TestNoWindowWhileReplacing(t *testing.T) {
	r := New()
	const registrations = 300

	var (
		wg          sync.WaitGroup
		stop        = make(chan struct{})
		absent      = make(chan uint32, 1) // highest port seen before a miss
		wentBack    = make(chan [2]uint32, 1)
		lookupsSeen int
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var highest uint32
		for {
			select {
			case <-stop:
				return
			default:
			}
			up, ok := r.Lookup("payments")
			if !ok {
				if highest != 0 {
					select {
					case absent <- highest:
					default:
					}
				}
				continue
			}
			lookupsSeen++
			if up.Port < highest {
				select {
				case wentBack <- [2]uint32{highest, up.Port}:
				default:
				}
			}
			highest = up.Port
		}
	}()

	var prev *Registration
	for i := 1; i <= registrations; i++ {
		h := mustRegister(t, r, Instance{Service: "payments", Port: uint32(1000 + i)})
		if prev != nil {
			prev.Close()
		}
		prev = h
	}
	close(stop)
	wg.Wait()

	select {
	case highest := <-absent:
		t.Errorf("payments disappeared from the registry mid-replacement (last seen port %d)", highest)
	default:
	}
	select {
	case pair := <-wentBack:
		t.Errorf("traffic moved backwards: saw port %d after %d — two builds are sharing the name", pair[1], pair[0])
	default:
	}
	if lookupsSeen == 0 {
		t.Fatal("the reader observed nothing; the test proved nothing")
	}
}

// TestConcurrentRegistrationsLeaveExactlyOneLive is the same guarantee under
// genuine contention rather than an interleaving the test chose.
func TestConcurrentRegistrationsLeaveExactlyOneLive(t *testing.T) {
	r := New()
	const instances = 50

	var wg sync.WaitGroup
	handles := make([]*Registration, instances)
	for i := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := r.Register(Instance{Service: "payments", Port: uint32(2000 + i), PID: int32(i)})
			if err != nil {
				t.Errorf("Register: %v", err)
				return
			}
			handles[i] = h
		}()
	}
	// Lookups run throughout, so -race sees reads against the writes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 2000 {
			r.Lookup("payments")
			r.LookupAuthority("payments.internal:9777")
		}
	}()
	wg.Wait()

	up, ok := r.Lookup("payments")
	if !ok {
		t.Fatal("nothing is registered after concurrent registration")
	}

	live := 0
	for _, h := range handles {
		if h == nil {
			continue
		}
		if _, evicted := h.EvictedBy(); !evicted {
			live++
			if h.Instance().Port != up.Port {
				t.Errorf("a handle that was never evicted (port %d) is not the one serving traffic (port %d)",
					h.Instance().Port, up.Port)
			}
		}
	}
	if live != 1 {
		t.Errorf("%d registrations believe they hold the name; exactly 1 may", live)
	}
	// min: raising `instances` past the history cap to stress contention
	// harder must not turn this into a failure pointed at the registry.
	if got, want := len(r.Evictions()), min(instances-1, maxEvictions); got != want {
		t.Errorf("Evictions() = %d, want %d — every deposed instance must be recorded", got, want)
	}
	if got := len(r.Services()); got != 1 {
		t.Errorf("Services() = %d entries, want 1", got)
	}
}

// TestRegisterWhileClosing is the third edge: a registration arriving while
// an eviction for that name is in flight.
func TestRegisterWhileClosing(t *testing.T) {
	r := New()
	const rounds = 200

	for i := range rounds {
		old := mustRegister(t, r, Instance{Service: "payments", Port: uint32(3000 + i)})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); old.Close() }()
		go func() {
			defer wg.Done()
			mustRegister(t, r, Instance{Service: "payments", Port: uint32(4000 + i)})
		}()
		wg.Wait()

		up, ok := r.Lookup("payments")
		if !ok {
			t.Fatalf("round %d: the name vanished when a close raced a registration", i)
		}
		if up.Port != uint32(4000+i) {
			t.Fatalf("round %d: Lookup = port %d, want the newest registration %d", i, up.Port, 4000+i)
		}
	}
}

func TestLookupIsAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector perturbs allocation accounting; run without -race (make test-short)")
	}
	r := New()
	mustRegister(t, r, Instance{Service: "payments", Port: 51234})

	if n := testing.AllocsPerRun(200, func() { r.Lookup("payments") }); n != 0 {
		t.Errorf("Lookup allocates %v times per call; it runs per request", n)
	}
	if n := testing.AllocsPerRun(200, func() { r.LookupAuthority("payments.internal:9777") }); n != 0 {
		t.Errorf("LookupAuthority allocates %v times per call; it runs per request", n)
	}
	// A miss is the diagnostic path and just as hot under a typo'd authority.
	if n := testing.AllocsPerRun(200, func() { r.LookupAuthority("paymnets.internal:9777") }); n != 0 {
		t.Errorf("a LookupAuthority miss allocates %v times per call", n)
	}
}

func TestEvictionHistoryIsBounded(t *testing.T) {
	r := New()
	total := maxEvictions + 20
	for i := range total {
		mustRegister(t, r, Instance{Service: "payments", Port: uint32(5000 + i)})
	}

	evictions := r.Evictions()
	if len(evictions) != maxEvictions {
		t.Fatalf("Evictions() = %d, want the cap %d", len(evictions), maxEvictions)
	}
	// Oldest first, and it is the newest evictions that were kept.
	if got, want := evictions[len(evictions)-1].Evicted.Port, uint32(5000+total-2); got != want {
		t.Errorf("newest retained eviction = port %d, want %d", got, want)
	}
	for i := 1; i < len(evictions); i++ {
		if evictions[i-1].Evicted.Port >= evictions[i].Evicted.Port {
			t.Fatalf("history is not oldest-first at %d: %+v", i, evictions)
		}
	}
}

// TestEvictionHistoryKeepsEveryServicesRecord: the history is capped, and the
// cap must not be spent entirely on whichever service is being restarted in a
// loop — that is ADR 004's main workflow, and the record it would flush out is
// the one ADR 005 keeps this history for.
func TestEvictionHistoryKeepsEveryServicesRecord(t *testing.T) {
	r := New()
	for i := range 2 {
		mustRegister(t, r, Instance{Service: "payments", Port: uint32(1000 + i)})
	}

	// A recompile loop on an unrelated service, long enough to fill the
	// history several times over.
	for i := range maxEvictions * 3 {
		mustRegister(t, r, Instance{Service: "billing", Port: uint32(2000 + i)})
	}

	var found bool
	for _, ev := range r.Evictions() {
		if ev.Evicted.Service == "payments" {
			found = true
		}
	}
	if !found {
		t.Errorf("payments' eviction was flushed out of the history by %d billing restarts", maxEvictions*3)
	}
	if got := len(r.Evictions()); got > maxEvictions {
		t.Errorf("Evictions() = %d records, above the cap %d", got, maxEvictions)
	}
}

// TestReRegisterFromSameProcessIsNotAnEviction is the stream flap ADR 004
// builds in: the client reconnects and re-registers before voya has torn down
// the stream it just lost. That is one process, so none of ADR 002's
// evict-and-replace machinery may fire — a warning naming the same pid twice
// sends the developer looking for a second build that does not exist, and a
// closed Evicted channel tells the process to stop re-registering (#27) for a
// name that is its own.
func TestReRegisterFromSameProcessIsNotAnEviction(t *testing.T) {
	warnings := 0
	r := New(WithEvictionHandler(func(Eviction) { warnings++ }))

	first := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 4242})
	second := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 4242})

	select {
	case <-first.Evicted():
		t.Error("a process was told it had been deposed by itself")
	default:
	}
	if _, evicted := first.EvictedBy(); evicted {
		t.Error("EvictedBy reported an eviction for a re-registration")
	}
	if warnings != 0 {
		t.Errorf("the eviction warning fired %d times for one process re-registering", warnings)
	}
	if got := len(r.Evictions()); got != 0 {
		t.Errorf("Evictions() = %d records, want 0 — a re-registration burned a history slot", got)
	}

	// The lost stream's teardown must not take the name from the stream that
	// replaced it, and Close stays idempotent: a second call must not count
	// for the other stream.
	first.Close()
	first.Close()
	if up, ok := r.Lookup("payments"); !ok || up.Port != 1001 {
		t.Errorf("the lost stream's Close deregistered the live one: %+v, ok=%v", up, ok)
	}
	second.Close()
	if up, ok := r.Lookup("payments"); ok {
		t.Errorf("the name outlived both streams: %+v", up)
	}

	// On one port, so that it is the pids deciding these. Two distinct processes
	// remain an eviction, and an unsupplied PID must not make two anonymous
	// instances look like one reconnecting.
	for _, pids := range [][2]int32{{11, 22}, {0, 0}} {
		r := New()
		h := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: pids[0]})
		mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: pids[1]})
		select {
		case <-h.Evicted():
		default:
			t.Errorf("pids %v: a genuine eviction went unsignalled", pids)
		}
	}
}

// TestAPIDOnAnotherPortIsAnEviction is #308's takeover: the pid is whatever the
// client says, so a process naming the holder's pid and a port of its own must
// not pass for a re-registration. Traffic moves to that port, and ADR 002
// allows that only loudly.
func TestAPIDOnAnotherPortIsAnEviction(t *testing.T) {
	var announced []Eviction
	r := New(WithEvictionHandler(func(e Eviction) { announced = append(announced, e) }))

	victim := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 4242})
	mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 4242})

	select {
	case <-victim.Evicted():
	default:
		t.Error("the holder was not told it lost the name")
	}
	if len(announced) != 1 {
		t.Fatalf("announced %d evictions, want 1", len(announced))
	}
	if got := len(r.Evictions()); got != 1 {
		t.Errorf("Evictions() = %d records, want 1: voya status is the record of this", got)
	}
	if up, ok := r.Lookup("payments"); !ok || up.Port != 1002 {
		t.Errorf("Lookup = %+v, ok=%v; want the incoming port 1002", up, ok)
	}

	// The careful path must read it the same way, or a deposed instance
	// retrying could not tell the name was held.
	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1003, PID: 4242}); !errors.Is(err, ErrNameHeld) {
		t.Errorf("RegisterIfFree with the holder's pid and another port = %v, want ErrNameHeld", err)
	}
}

// TestACopyOfTheHolderCannotFreeTheName is the takeover in two steps, which
// comparing ports alone does not stop: register as an exact copy of the holder,
// which evicts nobody, then hang up. If that hang-up removes the name, the third
// step finds it free, so traffic moves with no eviction to record and the real
// holder's stream, still open, is told nothing.
func TestACopyOfTheHolderCannotFreeTheName(t *testing.T) {
	var announced []Eviction
	r := New(WithEvictionHandler(func(e Eviction) { announced = append(announced, e) }))

	holder := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 4242})
	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 4242}).Close()

	if up, ok := r.Lookup("payments"); !ok || up.Port != 1001 {
		t.Fatalf("the copy's hang-up took the name from the holder: %+v, ok=%v", up, ok)
	}
	if len(announced) != 0 {
		t.Errorf("announced %d evictions for a copy that moved no traffic", len(announced))
	}

	mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 6666})
	select {
	case <-holder.Evicted():
	default:
		t.Error("the holder was not told it lost the name")
	}
	if len(announced) != 1 || len(r.Evictions()) != 1 {
		t.Errorf("announced %d and recorded %d evictions, want 1 of each", len(announced), len(r.Evictions()))
	}
}

// TestACopyOutlastsTheHolder pins what SECURITY.md says joining costs: a stream
// copying the holder cannot free the name, and that same rule means it keeps
// the name after the holder has gone. voya has no way to tell the two apart, so
// nothing is recorded, the entry goes on naming the holder, and an instance
// deposed earlier is refused its reclaim. This is a documented divergence from
// ADR 004's "the stream closes, the service is evicted", not a design to keep:
// change the assertions when that is decided.
func TestACopyOutlastsTheHolder(t *testing.T) {
	r := New()
	deposed, err := r.Register(Instance{Service: "payments", Port: 1000, PID: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer deposed.Close()
	holder := Instance{Service: "payments", Port: 1001, PID: 111}
	real, err := r.Register(holder)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := r.Register(holder)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	evictions := len(r.Evictions())

	real.Close()

	up, ok := r.Lookup("payments")
	if !ok || up.Port != holder.Port || up.PID != holder.PID {
		t.Fatalf("Lookup = %+v, %v: the copy keeps the holder's entry", up, ok)
	}
	if got := len(r.Evictions()); got != evictions {
		t.Errorf("evictions went from %d to %d: the holder's exit records nothing", evictions, got)
	}
	if _, err := r.RegisterIfFree(deposed.Instance()); !errors.Is(err, ErrNameHeld) {
		t.Errorf("a reclaim while the copy holds the name: %v, want ErrNameHeld", err)
	}
}

// TestAnEvictionReachesEveryStreamOfTheInstance: a reconnect overlapping the
// stream it lost leaves two handles on one registration, and either may be the
// live one.
func TestAnEvictionReachesEveryStreamOfTheInstance(t *testing.T) {
	r := New()
	first := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 4242})
	second := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 4242})
	mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 6666})

	for i, h := range []*Registration{first, second} {
		if _, evicted := h.EvictedBy(); !evicted {
			t.Errorf("stream %d was not told about the eviction", i)
		}
		h.Close()
	}
	if up, ok := r.Lookup("payments"); !ok || up.Port != 1002 {
		t.Errorf("the deposed streams' Close touched the winner: %+v, ok=%v", up, ok)
	}
}

// TestEvictionHandlerRunsOffTheLock: the handler is caller code that prints,
// and holding the write lock across it would stall every in-flight lookup
// behind a terminal write. Written so a regression fails rather than hangs.
func TestEvictionHandlerRunsOffTheLock(t *testing.T) {
	var r *Registry
	done := make(chan Eviction, 1)
	r = New(WithEvictionHandler(func(e Eviction) {
		// Deadlocks if this runs under the registry's write lock.
		if _, ok := r.Lookup("payments"); !ok {
			t.Error("the replacement was not visible when the handler ran")
		}
		done <- e
	}))

	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 11})
	go func() {
		if _, err := r.Register(Instance{Service: "payments", Port: 1002, PID: 22}); err != nil {
			t.Errorf("Register: %v", err)
		}
	}()

	select {
	case e := <-done:
		if e.Evicted.Port != 1001 || e.Incoming.Port != 1002 {
			t.Errorf("handler got %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the eviction handler never ran — it is being called under the registry lock")
	}
}

func TestNoEvictionHandlerOnFirstRegistration(t *testing.T) {
	calls := 0
	r := New(WithEvictionHandler(func(Eviction) { calls++ }))
	mustRegister(t, r, Instance{Service: "payments", Port: 1001})
	mustRegister(t, r, Instance{Service: "billing", Port: 1002})
	if calls != 0 {
		t.Errorf("eviction handler fired %d times for registrations that deposed nobody", calls)
	}
}

func TestServicesIsSortedAndComplete(t *testing.T) {
	r := New()
	for _, name := range []string{"reports", "payments", "billing"} {
		mustRegister(t, r, Instance{Service: name, Port: 6000})
	}
	got := r.Services()
	want := []string{"billing", "payments", "reports"}
	if len(got) != len(want) {
		t.Fatalf("Services() = %d entries, want %d", len(got), len(want))
	}
	for i, up := range got {
		if up.Service != want[i] {
			t.Errorf("Services()[%d] = %q, want %q — output feeding `voya status` must not reshuffle", i, up.Service, want[i])
		}
	}
}

func TestRegisterRejectsBadNames(t *testing.T) {
	cases := []struct {
		name   string
		expect string
	}{
		{"", "empty"},
		{"voya", "reserved"},
		{"Payments", "lowercase"},
		{"pay_ments", "lowercase"},
		{"pay ments", "lowercase"},
		{"payments.internal", "lowercase"},
		{"-payments", "hyphen"},
		{"payments-", "hyphen"},
		{strings.Repeat("a", maxNameLen+1), "63"},
	}
	r := New()
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			_, err := r.Register(Instance{Service: tc.name, Port: 1001})
			if err == nil {
				t.Fatal("Register accepted it")
			}
			var ne *NameError
			if !errors.As(err, &ne) {
				t.Fatalf("error is %T, want *NameError: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error does not explain the rule (%q):\n%v", tc.expect, err)
			}
		})
	}

	// The boundary cases the rules above must still allow.
	for _, name := range []string{"a", "payments", "payments-api", "s3", strings.Repeat("a", maxNameLen)} {
		if _, err := r.Register(Instance{Service: name, Port: 1001}); err != nil {
			t.Errorf("Register(%q) rejected a valid name: %v", name, err)
		}
	}
}

// TestRegisterRejectsUnusablePorts: the wire carries the port as a uint32, so
// a value no listener could have bound reaches the registry as an ordinary
// number. Rejected at registration, where the error names the service, rather
// than at every dial the proxy makes afterwards.
func TestRegisterRejectsUnusablePorts(t *testing.T) {
	cases := []struct {
		port   uint32
		expect string
	}{
		{0, "not 0"},
		{maxPort + 1, "65535"},
		{70000, "65535"},
		{1 << 31, "65535"},
	}
	r := New()
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.port), func(t *testing.T) {
			_, err := r.Register(Instance{Service: "payments", Port: tc.port})
			if err == nil {
				t.Fatalf("Register accepted port %d", tc.port)
			}
			var pe *PortError
			if !errors.As(err, &pe) {
				t.Fatalf("error is %T, want *PortError: %v", err, err)
			}
			if !strings.Contains(err.Error(), "payments") {
				t.Errorf("error does not name the service: %v", err)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error does not explain the rule (%q): %v", tc.expect, err)
			}
		})
	}

	// The boundary the rule must still allow.
	if _, err := r.Register(Instance{Service: "payments", Port: maxPort}); err != nil {
		t.Errorf("Register(port %d) rejected a bindable port: %v", maxPort, err)
	}
}

// TestEvictionStringSaysWhichInstanceLostTheName: ADR 002 requires the
// warning to name the evicted and the incoming instance, and — the part that
// makes it worth printing — to say which is which. The developer reading it
// is the one whose build just stopped receiving traffic, and a warning with
// the labels the wrong way round sends them to the process that is serving
// fine.
//
// Asserted on the whole labelled line rather than on the five values
// separately (#58). Every test in the repo used to match the ports and pids
// anywhere in the string, so swapping the two blocks in [Eviction.String] —
// telling the developer the usurper had stopped receiving traffic and the
// incumbent had taken over — passed all of them.
func TestEvictionStringSaysWhichInstanceLostTheName(t *testing.T) {
	// One pid on both lines: a process that re-registered on a new port before
	// voya saw its old stream end, or a forged pid. Telling that developer the
	// evicted instance is still running sends them after a second process that
	// may not exist.
	moved := Eviction{
		Evicted:  Instance{Service: "payments", Port: 1001, PID: 111},
		Incoming: Instance{Service: "payments", Port: 1002, PID: 111},
	}.String()
	if strings.Contains(moved, "still running") {
		t.Errorf("one pid on both lines must not claim a second running instance:\n%s", moved)
	}
	if !strings.Contains(moved, "Both name one pid") {
		t.Errorf("one pid on both lines must say so:\n%s", moved)
	}
	two := Eviction{
		Evicted:  Instance{Service: "payments", Port: 1001, PID: 111},
		Incoming: Instance{Service: "payments", Port: 1002, PID: 222},
	}.String()
	if !strings.Contains(two, "still running") || strings.Contains(two, "Both name one pid") {
		t.Errorf("two pids are two instances:\n%s", two)
	}
	// Two clients that reported no pid share a zero, not a process.
	anon := Eviction{
		Evicted:  Instance{Service: "payments", Port: 1001},
		Incoming: Instance{Service: "payments", Port: 1002},
	}.String()
	if strings.Contains(anon, "Both name one pid") {
		t.Errorf("no pid on either line is not one pid:\n%s", anon)
	}

	e := Eviction{
		Evicted:  Instance{Service: "payments", Port: 1001, PID: 111},
		Incoming: Instance{Service: "payments", Port: 1002, PID: 222},
	}
	got := e.String()
	for _, want := range []string{
		"payments.internal",
		"evicted: port 1001, pid 111",
		"incoming: port 1002, pid 222",
	} {
		if !strings.Contains(squeeze(got), want) {
			t.Errorf("eviction warning does not say %q:\n%s", want, got)
		}
	}

	// A client that reported no pid, which [Instance.PID] documents as a
	// supported case. The two `if PID != 0` guards are the one place left
	// where a pid can end up on the instance that did not report one, and
	// they are invisible to the block above: both of its instances have one.
	quiet := Eviction{
		Evicted:  Instance{Service: "payments", Port: 1001},
		Incoming: Instance{Service: "payments", Port: 1002, PID: 222},
	}
	quietly := squeeze(quiet.String())
	if !strings.Contains(quietly, "incoming: port 1002, pid 222") {
		t.Errorf("the pid that was reported did not stay with the instance that reported it:\n%s", quiet.String())
	}
	if strings.Contains(quietly, "pid 0") {
		t.Errorf("the warning sends the developer after pid 0 for a client that reported none:\n%s", quiet.String())
	}
}

// TestReclaimStringNamesTheInstanceNowServing is the same property on the
// other announcement. There is one instance rather than two, so the label
// cannot be attached to the wrong one — but it can be attached to the wrong
// *number*, since the reclaim also carries how long the name was lost for,
// and "now serving: port 1001" is the line that tells the developer which
// process traffic went back to.
func TestReclaimStringNamesTheInstanceNowServing(t *testing.T) {
	// A fixed instant, as everywhere else in this file: two time.Now calls
	// would put the wall-clock gap between them into a duration this test
	// then asserts is exactly 1m30s.
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	rc := Reclaim{
		Instance:  Instance{Service: "payments", Port: 1001, PID: 111},
		EvictedAt: at.Add(-90 * time.Second),
		At:        at,
	}
	got := rc.String()
	for _, want := range []string{
		"payments.internal",
		"now serving: port 1001, pid 111",
		"it lost the name 1m30s ago",
	} {
		if !strings.Contains(squeeze(got), want) {
			t.Errorf("reclaim announcement does not say %q:\n%s", want, got)
		}
	}

	// The other half of the same rendering: [Reclaim.EvictedAt] is documented
	// zero for a reclaim that follows no eviction, and the guard that keeps
	// the duration line off such an announcement is what stops it claiming
	// the name was lost for fifty-odd years.
	fresh := Reclaim{Instance: Instance{Service: "payments", Port: 1001, PID: 111}, At: at}
	if strings.Contains(squeeze(fresh.String()), "lost the name") {
		t.Errorf("a reclaim that followed no eviction says how long the name was lost:\n%s", fresh.String())
	}
}

// squeeze collapses every run of whitespace to one space, so that a test
// asserting on an announcement is about which instance carries which label
// and not about the column the labels are padded into — `evicted:` is given a
// second space to line up with `incoming:`, and the wants are written with
// one. ADR 002
// fixes what the warning has to say, not how it is laid out, so reflowing
// those columns must not fail here; swapping the instances is what has to.
//
// The collapsing is load-bearing rather than decoration, and the eviction
// test is what shows it: drop the squeeze from that call and its `evicted:`
// want stops matching the padded warning.
func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestKnownRemembersNamesThatAreNoLongerLive is what lets the proxy tell
// "your other terminal" from "you typed it wrong" (ADR 005). Without it a
// service whose stream dropped is indistinguishable from a typo, and those
// two failures want completely different advice.
func TestKnownRemembersNamesThatAreNoLongerLive(t *testing.T) {
	r := New()
	payments := mustRegister(t, r, Instance{Service: "payments", Port: 1001})
	mustRegister(t, r, Instance{Service: "billing", Port: 1002})
	payments.Close()

	if _, live := r.Lookup("payments"); live {
		t.Fatal("payments is still live")
	}
	want := []string{"billing", "payments"}
	got := r.Known()
	if len(got) != len(want) {
		t.Fatalf("Known() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Known()[%d] = %q, want %q — the list must be sorted for stable output", i, got[i], want[i])
		}
	}
	if _, live := r.Lookup("reports"); live {
		t.Fatal("reports was never registered")
	}
	if slices.Contains(r.Known(), "reports") {
		t.Error("Known() invented a name nobody registered")
	}
}

// TestKnowsAgreesWithKnown: the proxy asks Knows on every unroutable request
// and Known only when it is about to print the list, so the two must not be
// able to answer differently.
func TestKnowsAgreesWithKnown(t *testing.T) {
	r := New()
	handle := mustRegister(t, r, Instance{Service: "payments", Port: 1001})
	handle.Close()

	if !r.Knows("payments") {
		t.Error("Knows(payments) = false after it registered and stopped")
	}
	if r.Knows("reports") {
		t.Error("Knows(reports) = true for a name nobody registered")
	}
	for _, name := range r.Known() {
		if !r.Knows(name) {
			t.Errorf("Known() lists %q but Knows(%q) = false", name, name)
		}
	}
}

func TestKnownDoesNotDuplicateOnReRegistration(t *testing.T) {
	r := New()
	for port := range 5 {
		mustRegister(t, r, Instance{Service: "payments", Port: uint32(1000 + port)})
	}
	if got := r.Known(); len(got) != 1 {
		t.Errorf("Known() = %v, want one entry for five registrations of one name", got)
	}
}

// TestKnownIsBounded: the memory is for a dev session, and a process
// registering generated names must not grow it without limit.
func TestKnownIsBounded(t *testing.T) {
	r := New()
	total := maxSeen + 25
	for i := range total {
		mustRegister(t, r, Instance{Service: fmt.Sprintf("service-%03d", i), Port: 1001})
	}
	got := r.Known()
	if len(got) != maxSeen {
		t.Fatalf("Known() holds %d names, want the cap %d", len(got), maxSeen)
	}
	// The oldest are the ones dropped.
	if slices.Contains(got, "service-000") {
		t.Error("the oldest name survived the cap; the newest are the useful ones")
	}
	if !slices.Contains(got, fmt.Sprintf("service-%03d", total-1)) {
		t.Error("the newest name was dropped")
	}
}

// --- #27: reclaiming a name after being deposed ------------------------------

// TestRegisterIfFreeRefusesAHeldName is the whole point of the careful path:
// an instance that was deposed must lose, or the two swap the name forever at
// whatever interval they retry on.
func TestRegisterIfFreeRefusesAHeldName(t *testing.T) {
	r := New()
	mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 222})

	_, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111})
	if err == nil {
		t.Fatal("a careful claim took a name that was held")
	}
	if !errors.Is(err, ErrNameHeld) {
		t.Errorf("errors.Is(err, ErrNameHeld) = false: %v", err)
	}
	// And it says who has it, since the developer's next question is which
	// process to look at.
	if !strings.Contains(err.Error(), "1002") {
		t.Errorf("the refusal does not name the holder: %v", err)
	}

	up, _ := r.Lookup("payments")
	if up.Port != 1002 {
		t.Errorf("the holder changed to port %d", up.Port)
	}
	if got := len(r.Evictions()); got != 0 {
		t.Errorf("a refused claim recorded %d evictions", got)
	}
}

func TestRegisterIfFreeTakesAFreeName(t *testing.T) {
	r := New()
	handle := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})
	handle.Close()

	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("RegisterIfFree on a free name: %v", err)
	}
	if up, live := r.Lookup("payments"); !live || up.Port != 1001 {
		t.Errorf("Lookup = %+v, live=%v", up, live)
	}
}

// TestRegisterIfFreeAllowsTheSameProcess: an instance re-registering itself is
// not claiming against anybody, and refusing it would strand a service whose
// stream flapped.
func TestRegisterIfFreeAllowsTheSameProcess(t *testing.T) {
	r := New()
	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})

	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("a process was refused its own name: %v", err)
	}
}

// TestReclaimIsAnnounced: traffic went back to a build somebody replaced on
// purpose, and nothing asked for that. It has to be as loud as the eviction
// was, or option (b) trades a visible failure for a silent one.
func TestReclaimIsAnnounced(t *testing.T) {
	reclaims := make(chan Reclaim, 4)
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	r := New(
		WithClock(func() time.Time { return at }),
		WithReclaimHandler(func(rc Reclaim) { reclaims <- rc }),
	)

	deposed := mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})
	usurper := mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 222})
	select {
	case <-deposed.Evicted():
	default:
		t.Fatal("the first instance was not evicted")
	}
	usurper.Close() // whoever deposed it goes away

	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	select {
	case rc := <-reclaims:
		if rc.Instance.Port != 1001 {
			t.Errorf("reclaim names port %d, want 1001", rc.Instance.Port)
		}
		if rc.EvictedAt.IsZero() {
			t.Error("the reclaim does not know when the name was lost")
		}
		// On the labelled line, for #58's reason: this announcement carries a
		// duration as well as a port, so a bare "1001" anywhere in it is
		// satisfied by a rendering that put the number somewhere else.
		for _, want := range []string{"payments.internal", "now serving: port 1001, pid 111", "deposed"} {
			if !strings.Contains(squeeze(rc.String()), want) {
				t.Errorf("the announcement omits %q:\n%s", want, rc.String())
			}
		}
	default:
		t.Fatal("the reclaim was silent")
	}
}

// TestFirstRegistrationIsNotAReclaim: a careful claim on a name nobody has
// ever held announces nothing, because nothing happened.
func TestFirstRegistrationIsNotAReclaim(t *testing.T) {
	reclaims := 0
	r := New(WithReclaimHandler(func(Reclaim) { reclaims++ }))

	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("RegisterIfFree: %v", err)
	}
	if reclaims != 0 {
		t.Errorf("a first registration announced %d reclaims", reclaims)
	}
}

// TestAStrangerTakingAFreeNameIsNotAReclaim: the name was taken from
// somebody, but not from this instance. Announcing it would tell a developer
// who has replaced nothing that the build they replaced is serving again.
func TestAStrangerTakingAFreeNameIsNotAReclaim(t *testing.T) {
	reclaims := make(chan Reclaim, 4)
	r := New(WithReclaimHandler(func(rc Reclaim) { reclaims <- rc }))

	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})
	usurper := mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 222})
	usurper.Close()

	// A third instance, which nobody ever deposed, takes the free name.
	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1003, PID: 333}); err != nil {
		t.Fatalf("RegisterIfFree: %v", err)
	}
	select {
	case rc := <-reclaims:
		t.Errorf("an instance that was never deposed was announced as a reclaim:\n%s", rc.String())
	default:
	}
}

// TestReclaimIsTimedFromItsOwnEviction: with more than one instance deposed
// from a name, "it lost the name %s ago" has to mean the instance that came
// back, not the last one to lose the name.
func TestReclaimIsTimedFromItsOwnEviction(t *testing.T) {
	reclaims := make(chan Reclaim, 4)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	r := New(
		WithClock(func() time.Time { return now }),
		WithReclaimHandler(func(rc Reclaim) { reclaims <- rc }),
	)

	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})
	now = now.Add(time.Minute) // 12:01: the first instance is deposed
	mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 222})
	now = now.Add(time.Minute) // 12:02: and so is the second
	third := mustRegister(t, r, Instance{Service: "payments", Port: 1003, PID: 333})
	third.Close()

	now = now.Add(time.Minute) // 12:03: the first one asks for it back
	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	select {
	case rc := <-reclaims:
		if got := rc.At.Sub(rc.EvictedAt); got != 2*time.Minute {
			t.Errorf("the reclaim is timed from somebody else's eviction: %s, want 2m0s", got)
		}
		if !strings.Contains(squeeze(rc.String()), "it lost the name 2m0s ago") {
			t.Errorf("the announcement misreports how long the name was lost:\n%s", rc.String())
		}
	default:
		t.Fatal("the reclaim was silent")
	}
}

// TestReRegisteringTheSameProcessIsNotAReclaim: the careful path succeeds for
// the process that already holds the name (it is claiming against nobody), but
// nothing moved, so there is nothing to announce.
func TestReRegisteringTheSameProcessIsNotAReclaim(t *testing.T) {
	reclaims := make(chan Reclaim, 4)
	r := New(WithReclaimHandler(func(rc Reclaim) { reclaims <- rc }))

	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})
	usurper := mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 222})
	usurper.Close()

	// The deposed instance takes its name back: announced, once.
	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	select {
	case <-reclaims:
	default:
		t.Fatal("the reclaim was silent")
	}

	// It then re-registers over itself, which takes nothing back from anybody.
	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("re-registration: %v", err)
	}
	select {
	case rc := <-reclaims:
		t.Errorf("a process re-registering its own name announced a reclaim:\n%s", rc.String())
	default:
	}
}

// --- #149: the reclaim history `voya status` reads --------------------------

// TestReclaimsAreRetainedForStatus: the record the reclaim handler is handed
// is also kept, the way evictions are, so `voya status` can show a name that
// went back to a replaced build after the announcement has scrolled away
// (ADR 002, ADR 005). Same value both ways, so the two cannot describe one
// reclaim differently.
func TestReclaimsAreRetainedForStatus(t *testing.T) {
	announced := make(chan Reclaim, 4)
	at := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	r := New(
		WithClock(func() time.Time { return at }),
		WithReclaimHandler(func(rc Reclaim) { announced <- rc }),
	)
	if got := r.Reclaims(); len(got) != 0 {
		t.Fatalf("a fresh registry has %d reclaims", len(got))
	}

	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})
	usurper := mustRegister(t, r, Instance{Service: "payments", Port: 1002, PID: 222})
	usurper.Close()
	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	var handed Reclaim
	select {
	case handed = <-announced:
	default:
		t.Fatal("the reclaim was silent, so there is nothing to compare the record against")
	}
	kept := r.Reclaims()
	if len(kept) != 1 {
		t.Fatalf("Reclaims() = %d records, want 1", len(kept))
	}
	if kept[0] != handed {
		t.Errorf("the retained record differs from the announced one:\n kept:  %+v\n handed: %+v", kept[0], handed)
	}
	if kept[0].Instance.Port != 1001 || kept[0].EvictedAt.IsZero() {
		t.Errorf("the record is incomplete: %+v", kept[0])
	}

	// A copy, not the registry's own slice: a reader that appends to what it
	// was handed must not be writing into the history.
	kept[0].Instance.Port = 9
	if again := r.Reclaims(); again[0].Instance.Port != 1001 {
		t.Error("Reclaims() hands out the registry's own storage")
	}
}

// TestReclaimHistoryIsOldestFirstAndCapped: the same shape as the eviction
// history's, and spent oldest-first — the comment on maxReclaims says why
// that is enough here where it was not for evictions.
func TestReclaimHistoryIsOldestFirstAndCapped(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	r := New(WithClock(func() time.Time { return now }))

	// One instance, deposed and reclaiming, maxReclaims+3 times over: each
	// round is a fresh eviction, so each careful re-registration is a reclaim.
	for i := range maxReclaims + 3 {
		now = now.Add(time.Second)
		usurper := mustRegister(t, r, Instance{Service: "payments", Port: 2000, PID: 222})
		usurper.Close()
		now = now.Add(time.Second)
		if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
			t.Fatalf("round %d: reclaim: %v", i, err)
		}
	}
	// Round 0's careful claim found a free name nobody had taken from it, so
	// it announced nothing; every round after it deposes the reclaimed
	// instance first, so maxReclaims+2 reclaims happened and the cap is what
	// is left.
	got := r.Reclaims()
	if len(got) != maxReclaims {
		t.Fatalf("Reclaims() = %d, want the cap %d", len(got), maxReclaims)
	}
	for i := 1; i < len(got); i++ {
		if !got[i].At.After(got[i-1].At) {
			t.Fatalf("history is not oldest-first at %d: %v then %v", i, got[i-1].At, got[i].At)
		}
	}
	// And it is the newest that survive: the last record is the last round.
	if got[len(got)-1].At != now {
		t.Errorf("the newest reclaim was dropped: last At = %v, want %v", got[len(got)-1].At, now)
	}
}

// TestSnapshotIsTheThreeReadersAtOnce: on a registry nobody is writing to,
// [Registry.Snapshot] is exactly what the three accessors answer — same
// records, same order, services sorted by name. What it adds is that the
// three are read under one lock, which is internal/diag's
// TestGetStatusIsOneSnapshot to prove; here the contract is that taking them
// together does not change any of them.
func TestSnapshotIsTheThreeReadersAtOnce(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	r := New(WithClock(func() time.Time { return now }))
	mustRegister(t, r, Instance{Service: "payments", Port: 1001, PID: 111})
	mustRegister(t, r, Instance{Service: "billing", Port: 1002, PID: 222})
	now = now.Add(time.Minute)
	usurper := mustRegister(t, r, Instance{Service: "payments", Port: 1003, PID: 333})
	usurper.Close()
	now = now.Add(time.Minute)
	if _, err := r.RegisterIfFree(Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	snap := r.Snapshot()
	if !slices.Equal(snap.Services, r.Services()) {
		t.Errorf("Snapshot().Services = %+v, want Services() %+v", snap.Services, r.Services())
	}
	if !slices.Equal(snap.Evictions, r.Evictions()) {
		t.Errorf("Snapshot().Evictions = %+v, want Evictions() %+v", snap.Evictions, r.Evictions())
	}
	if !slices.Equal(snap.Reclaims, r.Reclaims()) {
		t.Errorf("Snapshot().Reclaims = %+v, want Reclaims() %+v", snap.Reclaims, r.Reclaims())
	}
	// Not vacuous: all three carry something, and the services are ordered.
	if len(snap.Services) != 2 || len(snap.Evictions) != 1 || len(snap.Reclaims) != 1 {
		t.Fatalf("the staging is thin: %d services, %d evictions, %d reclaims",
			len(snap.Services), len(snap.Evictions), len(snap.Reclaims))
	}
	if snap.Services[0].Service != "billing" || snap.Services[1].Service != "payments" {
		t.Errorf("Snapshot().Services is not ordered by name: %+v", snap.Services)
	}

	// A copy, the way the three accessors hand out copies.
	snap.Services[0].Port = 9
	if again := r.Snapshot(); again.Services[0].Port == 9 {
		t.Error("Snapshot hands out the registry's own storage")
	}
}
