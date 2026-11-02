// Package registry is voya's single source of truth for which service is
// listening on which ephemeral port.
//
// In memory only. ADR 004 rejects persistence: services re-register when
// their stream breaks, so a restart heals itself, and a stale registry on
// disk would be a new failure mode that outlives the process that wrote it.
//
// The registry does not know what a stream is. A caller registers an
// instance, holds the returned [Registration] for as long as its stream is
// open, and calls Close when it ends; if another instance takes the name in
// the meantime, the handle's [Registration.Evicted] channel closes and the
// caller does the protocol part (ADR 002's warning, and #27's "ask for the
// name back rather than take it" — see [Registry.RegisterIfFree]). Keeping the
// transport out of here is what lets the evict-and-replace rules be tested
// without a server.
package registry

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Reserved is the one name no service may take: ADR 005 puts voya's own
// programmatic diagnostics under voya.internal, and a service answering there
// would shadow the endpoint a developer reaches for when routing is already
// broken.
const Reserved = "voya"

// maxEvictions is how many evictions are kept for `voya status`.
//
// ADR 005 wants evictions surfaced after the fact, because the developer who
// needs one is the developer whose instance was deposed and whose warning has
// long scrolled away. It also rejects unbounded retention: a dev tool's
// history is worthless after the session that produced it. A session's worth
// of restarts is tens, not thousands.
//
// The cap is on the whole history, but it is not spent first-come — see
// [Registry.trimHistory] for which record a full history gives up.
const maxEvictions = 64

// maxReclaims is how many reclaims are kept for `voya status`, for the reason
// [maxEvictions] gives: the reader is the developer whose replaced build is
// serving traffic again and whose announcement has scrolled away (#27, ADR
// 005). The same number, because a reclaim follows an eviction and there
// cannot be more of them than there were of those worth remembering. Spent
// oldest-first: nothing produces reclaims at the rate a recompile loop
// produces evictions, so there is no restart loop of one name for another
// name's record to survive.
const maxReclaims = maxEvictions

// maxSeen bounds the names [Registry.Known] remembers.
//
// A name voya has heard of but is not serving right now is a different
// failure from a name nobody ever registered — the first is "your other
// terminal", the second is "you typed it wrong" — and ADR 005 wants the
// proxy's diagnostic to tell them apart. That needs a memory of names beyond
// what is live, and the memory has to stop somewhere: a dev session runs tens
// of services, so anything past this is a process that is registering
// generated names, and forgetting the oldest is the right answer for it.
const maxSeen = 128

// maxPort is the highest port a TCP listener can bind. Registration carries
// the port in a proto uint32 (`voya.registry.v1.RegisterRequest.port`), which
// is four times wider than a port number, so the range has to be checked
// here rather than assumed from the type.
//
// internal/config keeps its own copy for voya's own three listeners. Not
// shared: that one is about a number a developer typed into a .pxf layer,
// this one about a number a client process reported over the wire, and
// neither package should have to import the other to reject 70000.
const maxPort = 65535

// Instance identifies one running copy of a service.
type Instance struct {
	// Service is the bare name — "payments", not "payments.internal". It is
	// what FLY_APP_NAME holds in production.
	Service string

	// Port is the port the service actually bound after asking for 0.
	Port uint32

	// PID is the registering process as the client reports it, which voya
	// cannot check. An eviction warning names it so the developer has
	// something to act on (ADR 002), and with the port it decides whether a
	// registration is the holder reconnecting (see [reconnecting]) and whether
	// a claim is a reclaim. Zero when the client did not supply one.
	PID int32
}

// Upstream is a lookup result: where to send a request for a service.
//
// Returned by value. Lookup is the proxy's per-request hot path, and handing
// back a pointer into the registry's own state would both allocate and let a
// caller mutate the routing table by accident.
type Upstream struct {
	Service string
	Port    uint32
	PID     int32

	// Since is when this instance registered — not when the service was
	// first seen. A restart resets it, which is what `voya status`' uptime
	// column should show.
	Since time.Time
}

// Eviction records one instance losing its name to another (ADR 002).
type Eviction struct {
	Evicted  Instance
	Incoming Instance
	At       time.Time
}

// NameError reports a service name the registry will not accept.
type NameError struct {
	Name   string
	Reason string
}

func (e *NameError) Error() string {
	return fmt.Sprintf("registry: cannot register %q: %s", e.Name, e.Reason)
}

// PortError reports a port the registry will not accept.
type PortError struct {
	Service string
	Port    uint32
	Reason  string
}

func (e *PortError) Error() string {
	return fmt.Sprintf("registry: cannot register %q on port %d: %s", e.Service, e.Port, e.Reason)
}

// ErrNameHeld is returned by [Registry.RegisterIfFree] when another instance
// holds the name, which may be the same process on another port. It is not a
// failure of the caller's registration so much as its answer: the name is
// legitimately somebody else's.
var ErrNameHeld = errors.New("registry: the name is held by another instance")

// Reclaim records a deposed instance taking its name back after whoever
// deposed it went away (#27).
type Reclaim struct {
	Instance Instance

	// EvictedAt is when this instance was deposed, zero if it never was — not
	// when the name was last taken from anybody, which would be somebody
	// else's eviction whenever more than two instances have fought over one
	// name. A reclaim that follows no eviction is just a registration that
	// used the careful path.
	EvictedAt time.Time

	At time.Time
}

// String renders the announcement a reclaim has to make.
//
// It has to be as prominent as the eviction warning was. The developer who
// needs it replaced an instance *because it was wrong*, and traffic has just
// gone back to it — silently returning to a known-bad build is the same
// twenty minutes of confusion ADR 002's warning exists to prevent, arrived at
// from the other side.
func (r Reclaim) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s went back to the instance that was deposed earlier", Authority(r.Instance.Service))
	fmt.Fprintf(&b, "\n  now serving: port %d", r.Instance.Port)
	if r.Instance.PID != 0 {
		fmt.Fprintf(&b, ", pid %d", r.Instance.PID)
	}
	if !r.EvictedAt.IsZero() {
		fmt.Fprintf(&b, "\n  it lost the name %s ago, and whatever replaced it is gone",
			r.At.Sub(r.EvictedAt).Round(time.Second))
	}
	b.WriteString("\nIf you replaced it because it was wrong, that is the build serving traffic again.")
	return b.String()
}

// Option configures a Registry.
type Option func(*Registry)

// WithClock replaces time.Now, for tests that assert on timestamps.
func WithClock(now func() time.Time) Option {
	return func(r *Registry) { r.now = now }
}

// WithReclaimHandler registers a callback fired when a deposed instance takes
// its name back, off the registry's lock. The counterpart to
// [WithEvictionHandler], and needed for the same reason: traffic moved, and
// nobody asked for it to.
func WithReclaimHandler(fn func(Reclaim)) Option {
	return func(r *Registry) { r.onReclaim = fn }
}

// WithEvictionHandler registers a callback fired once per eviction, off the
// registry's lock.
//
// This is where ADR 002's "loudly" is actually delivered: the daemon wires
// the warning here. The registry keeps the history for `voya status`; making
// the noise is somebody else's job, because what counts as prominent depends
// on whether anyone is looking at a terminal.
func WithEvictionHandler(fn func(Eviction)) Option {
	return func(r *Registry) { r.onEvict = fn }
}

// Registry maps service names to the instance currently serving them.
type Registry struct {
	// A plain RWMutex, not a copy-on-write snapshot. Lookup under a read
	// lock is allocation-free and uncontended in the common case — a laptop
	// runs tens of services, not thousands — and a swap-a-new-map scheme
	// would trade that for a full map copy on every restart in a
	// recompile loop.
	mu       sync.RWMutex
	live     map[string]*entry
	seen     []string   // every name ever registered, oldest first, capped
	history  []Eviction // newest last, capped at maxEvictions
	reclaims []Reclaim  // newest last, capped at maxReclaims
	nextGen  uint64

	now       func() time.Time
	onEvict   func(Eviction)
	onReclaim func(Reclaim)
}

// entry is one live registration. Held by pointer so the handle and the map
// refer to the same generation.
type entry struct {
	inst  Instance
	since time.Time
	gen   uint64

	// evicted closes when another instance takes this name. Signalled by a
	// close rather than a callback so a stream handler can select on it
	// alongside its own reads, and so every watcher sees it.
	evicted chan struct{}

	// eviction is the record of losing the name, written once before evicted
	// closes so that close is the happens-before edge every reader of it
	// crosses. The incoming instance is in there — not kept a second time —
	// so the wire event, `voya status` and [Registration.EvictedBy] cannot
	// describe one eviction differently.
	eviction Eviction

	// streams counts the open registrations sharing this entry. More than one
	// means the instance reconnected before voya noticed the stream it lost
	// (see [reconnecting]). The name goes when the last of them closes, so a
	// stream that merely copies the holder's details cannot free it by hanging
	// up (#308).
	streams int
}

// New returns an empty registry.
func New(opts ...Option) *Registry {
	r := &Registry{
		live: make(map[string]*entry),
		now:  time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Registration is a live registration. The caller holds it for as long as its
// stream is open.
type Registration struct {
	reg  *Registry
	name string
	gen  uint64
	e    *entry

	// closed makes Close count once per handle. Guarded by reg.mu.
	closed bool
}

// Evicted closes when another instance of the same service takes the name.
// It never closes for any other reason — in particular not when the holder
// calls Close.
//
// The distinction is load-bearing. ADR 004 has a service whose stream drops
// reconnect and re-register, which is right when voya restarted; doing that
// after an eviction makes two live instances depose each other in a loop
// (#27). A caller that watches this channel can tell the two apart.
func (h *Registration) Evicted() <-chan struct{} { return h.e.evicted }

// EvictedBy names the instance that took the name over, and reports false
// while the registration is still live.
func (h *Registration) EvictedBy() (Instance, bool) {
	select {
	case <-h.e.evicted:
		return h.e.eviction.Incoming, true
	default:
		return Instance{}, false
	}
}

// Eviction returns the record the registry kept of this registration losing
// its name, and reports false while the registration is still live.
//
// The same value the eviction handler received and the same one
// [Registry.Evictions] retains — including [Eviction.At], which a caller
// cannot reconstruct afterwards. Anything that describes an eviction to
// somebody (the registration stream's Evicted event, `voya status`) renders
// this rather than assembling a partial copy, so one eviction cannot be
// described two ways.
func (h *Registration) Eviction() (Eviction, bool) {
	select {
	case <-h.e.evicted:
		return h.e.eviction, true
	default:
		return Eviction{}, false
	}
}

// Instance reports what was registered.
func (h *Registration) Instance() Instance { return h.e.inst }

// Close deregisters the instance once its last stream has ended. The caller
// invokes it when its stream ends, for any reason.
//
// Idempotent, and generation-checked: a handle that was already evicted
// removes nothing. That is the difference between a slow shutdown and an
// outage — the deposed instance's stream teardown races the replacement's
// registration, and without the check the loser's Close would delete the
// winner's entry and leave the name absent with a live service behind it.
//
// Counted, because a reconnect leaves one entry with two handles (see
// [reconnecting]). Without the count, whichever stream ended first would take
// the name from the one still open. A copy of the holder could then free the
// name by hanging up and claim it with no eviction to record (#308).
func (h *Registration) Close() {
	r := h.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	h.e.streams--
	if cur, ok := r.live[h.name]; ok && cur.gen == h.gen && h.e.streams == 0 {
		delete(r.live, h.name)
	}
}

// Register makes inst routable under its name, deposing whatever held that
// name before (ADR 002: evict-and-replace, loudly) — unless the holder is inst
// itself reconnecting, which deposes nobody and moves no traffic (see
// [reconnecting]).
//
// Replacement is atomic: the name never becomes momentarily absent, so a
// service restarting into its own still-registered entry cannot produce a
// window where requests to it fail.
func (r *Registry) Register(inst Instance) (*Registration, error) {
	return r.register(inst, false)
}

// RegisterIfFree registers inst only if nothing else holds the name,
// returning [ErrNameHeld] rather than evicting the holder.
//
// This is the careful half of the pair. [Register] is last-writer-wins
// because a service restarting has to take its name back from the copy that
// has not finished dying; an instance that was *deposed* needs the opposite,
// or the two swap the name forever at whatever interval they retry on (#27).
//
// A registration from the instance that already holds the name succeeds: that
// is a reconnect, not a claim against anyone.
func (r *Registry) RegisterIfFree(inst Instance) (*Registration, error) {
	return r.register(inst, true)
}

func (r *Registry) register(inst Instance, onlyIfFree bool) (*Registration, error) {
	if err := ValidName(inst.Service); err != nil {
		return nil, err
	}
	if err := validPort(inst); err != nil {
		return nil, err
	}

	r.mu.Lock()
	// One lookup for every question this needs to answer, so that the rule
	// for what counts as displacing somebody cannot come to differ between
	// the refusal below and the eviction further down. Past the reconnect,
	// holding the name means being displaced.
	prev, held := r.live[inst.Service]
	if held && reconnecting(prev.inst, inst) {
		prev.streams++
		r.mu.Unlock()
		return &Registration{reg: r, name: inst.Service, gen: prev.gen, e: prev}, nil
	}
	if onlyIfFree && held {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: %s is served by port %d",
			ErrNameHeld, Authority(inst.Service), prev.inst.Port)
	}
	now := r.now()
	r.nextGen++
	e := &entry{inst: inst, since: now, gen: r.nextGen, evicted: make(chan struct{}), streams: 1}

	var ev *Eviction
	if held {
		ev = &Eviction{Evicted: prev.inst, Incoming: inst, At: now}
		prev.eviction = *ev
		close(prev.evicted)
		r.history = append(r.history, *ev)
		r.trimHistory()
	}
	// A careful claim on a free name is a reclaim, provided this instance is
	// the one the history says lost it. Checked against the record of its own
	// eviction rather than a flag, so a first-ever registration on this path
	// announces nothing.
	var reclaim *Reclaim
	if onlyIfFree && !held {
		if evictedAt, deposed := r.lastEvictionOf(inst); deposed {
			reclaim = &Reclaim{Instance: inst, EvictedAt: evictedAt, At: now}
			r.reclaims = append(r.reclaims, *reclaim)
			if len(r.reclaims) > maxReclaims {
				r.reclaims = r.reclaims[len(r.reclaims)-maxReclaims:]
			}
		}
	}

	r.live[inst.Service] = e
	r.remember(inst.Service)
	onEvict, onReclaim := r.onEvict, r.onReclaim
	r.mu.Unlock()

	// Outside the lock: the handler is arbitrary caller code — it prints,
	// and one day may do more — and running it under the lock would stall
	// every in-flight lookup behind a terminal write.
	if ev != nil && onEvict != nil {
		onEvict(*ev)
	}
	if reclaim != nil && onReclaim != nil {
		onReclaim(*reclaim)
	}
	return &Registration{reg: r, name: inst.Service, gen: e.gen, e: e}, nil
}

// lastEvictionOf reports when this instance was last deposed. Caller holds the
// lock.
//
// Matched against the instance rather than the name alone. A third process
// taking a name somebody else was deposed from has got nothing back, and
// telling its developer that the build they replaced is serving again would
// be a warning about something that did not happen; and where several
// instances have been deposed from one name, this instance's own eviction is
// the only one that makes "it lost the name %s ago" true.
//
// Bounded by the eviction history, so an eviction old enough to have been
// trimmed reads as "never" and the reclaim goes unannounced. That is the
// right way round: a name evicted 64 evictions ago is not what anybody is
// currently confused about.
func (r *Registry) lastEvictionOf(inst Instance) (time.Time, bool) {
	for i := len(r.history) - 1; i >= 0; i-- {
		if sameInstance(r.history[i].Evicted, inst) {
			return r.history[i].At, true
		}
	}
	return time.Time{}, false
}

// sameInstance reports whether two records describe the same running copy of a
// service.
//
// The pid identifies it wherever the client supplied one. Without one the port
// is all there is, and it is enough: a deposed instance still holds its
// listener — that is the whole point of it still being able to serve — so no
// other process can be reporting that port.
func sameInstance(a, b Instance) bool {
	return a.Service == b.Service && a.Port == b.Port && a.PID == b.PID
}

// reconnecting reports whether an incoming registration is the instance that
// already holds the name, back on a new stream.
//
// That is the case ADR 004 builds in: a stream flaps, the client reconnects
// with backoff and re-registers, and it wins the race against voya's teardown
// of the stream it just lost. Treating that as an eviction is wrong twice over.
// ADR 002's warning exists for two builds racing for one name, and one that
// names the same pid on both lines sends the developer hunting a second process
// that does not exist; and closing the old handle's [Registration.Evicted] tells
// that process the name is legitimately someone else's (#27) when in fact it is
// its own.
//
// The port has to match as well as the pid. The pid is a number the client
// states and voya cannot check, so on its own it let any local process take a
// name silently by quoting the holder's (#308). A reconnect brings the port it
// had, and any other port moves traffic, which ADR 002 allows only loudly.
//
// A match is safe to accept without any proof of identity because it joins the
// existing entry rather than replacing it: traffic stays where it was, and the
// holder stays registered however the newcomer's stream ends.
//
// A PID of zero means the client declined to supply one, which is allowed —
// two anonymous registrations cannot be told apart, so they take the eviction
// path.
func reconnecting(incumbent, incoming Instance) bool {
	return incumbent.PID != 0 && sameInstance(incumbent, incoming)
}

// trimHistory drops records until the history is back under maxEvictions,
// taking each one from whichever service currently holds the most.
//
// Dropping the oldest record overall — the obvious form — loses the reader
// this history exists for. ADR 004 calls the recompile loop the main
// workflow, and a service in one produces evictions faster than everything
// else combined: a few dozen restarts of `billing` would flush the `payments`
// eviction that a developer is about to run `voya status` to find. Trimming
// the most-represented service instead means a name's most recent eviction
// survives anybody else's restart loop.
func (r *Registry) trimHistory() {
	for len(r.history) > maxEvictions {
		counts := make(map[string]int, len(r.history))
		for _, ev := range r.history {
			counts[ev.Evicted.Service]++
		}
		// history is oldest first, so scanning it in order breaks a tie
		// toward the service whose oldest record is oldest.
		drop, most := 0, 0
		for i, ev := range r.history {
			if n := counts[ev.Evicted.Service]; n > most {
				drop, most = i, n
			}
		}
		r.history = slices.Delete(r.history, drop, drop+1)
	}
}

// validPort reports whether a service may register on the port it reported.
func validPort(inst Instance) error {
	switch {
	case inst.Port == 0:
		return &PortError{
			Service: inst.Service,
			Port:    inst.Port,
			Reason:  "a service must report the port it actually bound, not 0",
		}
	case inst.Port > maxPort:
		// Reachable: the wire type is a uint32. Caught here so the failure
		// names the service, rather than surfacing later as an opaque dial
		// error on every request the proxy forwards (ADR 005).
		return &PortError{
			Service: inst.Service,
			Port:    inst.Port,
			Reason:  fmt.Sprintf("above the highest port number %d", maxPort),
		}
	}
	return nil
}

// remember records a name in the seen list. Caller holds the write lock.
func (r *Registry) remember(service string) {
	if slices.Contains(r.seen, service) {
		return
	}
	r.seen = append(r.seen, service)
	if len(r.seen) > maxSeen {
		r.seen = r.seen[len(r.seen)-maxSeen:]
	}
}

// Known lists the service names this voya has registered since it started,
// live or not, ordered by name — the most recent [maxSeen] of them, since the
// memory is bounded.
//
// This is what lets a diagnostic separate "registered earlier, not running
// now" from "never heard of it" (ADR 005). It is memory, not persistence: a
// voya restart forgets, which is correct, because a restart also means every
// service is about to re-register.
//
// Use [Registry.Knows] to ask about one name. This one sorts and copies, and
// a membership test wants neither.
func (r *Registry) Known() []string {
	r.mu.RLock()
	out := slices.Clone(r.seen)
	r.mu.RUnlock()
	slices.Sort(out)
	return out
}

// Knows reports whether a name is one voya has registered since it started.
//
// The question [Registry.Known] gets asked on the proxy's diagnostic path,
// where the answer decides between two error messages and the list itself is
// never read. Answering it here is one read lock and no allocation.
func (r *Registry) Knows(service string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Contains(r.seen, service)
}

// Lookup returns the instance currently serving a bare service name.
func (r *Registry) Lookup(service string) (Upstream, bool) {
	r.mu.RLock()
	e, ok := r.live[service]
	if !ok {
		r.mu.RUnlock()
		return Upstream{}, false
	}
	up := Upstream{Service: e.inst.Service, Port: e.inst.Port, PID: e.inst.PID, Since: e.since}
	r.mu.RUnlock()
	return up, true
}

// LookupAuthority is Lookup keyed by an :authority or Host header —
// "payments.internal:9777". Reports false for a name that is not voya's to
// route, which the proxy turns into a diagnostic rather than a bare 502
// (ADR 005).
func (r *Registry) LookupAuthority(authority string) (Upstream, bool) {
	name, ok := ServiceFromAuthority(authority)
	if !ok {
		return Upstream{}, false
	}
	return r.Lookup(name)
}

// Services lists every live registration, ordered by name.
//
// Sorted because this feeds `voya status` and `voya routes`, where map order
// would reshuffle the output between two runs and make a diff useless.
func (r *Registry) Services() []Upstream {
	r.mu.RLock()
	out := r.services()
	r.mu.RUnlock()
	return byName(out)
}

// services copies the live registrations, unsorted. Caller holds the lock.
func (r *Registry) services() []Upstream {
	out := make([]Upstream, 0, len(r.live))
	for _, e := range r.live {
		out = append(out, Upstream{
			Service: e.inst.Service, Port: e.inst.Port, PID: e.inst.PID, Since: e.since,
		})
	}
	return out
}

// byName sorts a copy of the live registrations, outside the lock: the order
// is for a reader, and no registration is waiting on it.
func byName(out []Upstream) []Upstream {
	slices.SortFunc(out, func(a, b Upstream) int { return strings.Compare(a.Service, b.Service) })
	return out
}

// Snapshot is everything `voya status` reads — the live registrations and
// both histories — as they were at one instant (#149).
//
// One lock for the three, because they are one question. Asked as three
// calls, an eviction landing between two of them tears the answer: the
// deposed instance is still in the services list the first call returned and
// the record deposing it is already in the history the second one does, so
// `voya status` shows a port serving traffic and, underneath, the record
// saying that port lost the name a moment ago. Either state is true on its
// own; the pair never was.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	snap := Snapshot{
		Services:  r.services(),
		Evictions: append([]Eviction(nil), r.history...),
		Reclaims:  append([]Reclaim(nil), r.reclaims...),
	}
	r.mu.RUnlock()
	snap.Services = byName(snap.Services)
	return snap
}

// Snapshot is the registry as one reader saw it: [Registry.Services],
// [Registry.Evictions] and [Registry.Reclaims], consistent with each other.
type Snapshot struct {
	Services  []Upstream // ordered by name
	Evictions []Eviction // oldest first
	Reclaims  []Reclaim  // oldest first
}

// Evictions returns the retained eviction history, oldest first.
func (r *Registry) Evictions() []Eviction {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Eviction(nil), r.history...)
}

// Reclaims returns the retained reclaim history, oldest first — every record
// the reclaim handler was handed, kept for `voya status` the way [Registry.
// Evictions] keeps the other half of a name changing hands (#149).
func (r *Registry) Reclaims() []Reclaim {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Reclaim(nil), r.reclaims...)
}

// String renders an eviction as the warning ADR 002 asks for: it has to name
// both instances, because the developer reading it is the one whose build
// just stopped receiving traffic and who needs to know which process to go
// and look at.
func (e Eviction) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s was taken over by another instance", Authority(e.Evicted.Service))
	fmt.Fprintf(&b, "\n  evicted:  port %d", e.Evicted.Port)
	if e.Evicted.PID != 0 {
		fmt.Fprintf(&b, ", pid %d", e.Evicted.PID)
	}
	fmt.Fprintf(&b, "\n  incoming: port %d", e.Incoming.Port)
	if e.Incoming.PID != 0 {
		fmt.Fprintf(&b, ", pid %d", e.Incoming.PID)
	}
	if e.Evicted.PID != 0 && e.Evicted.PID == e.Incoming.PID {
		// There may be no second process to go and look for. A pid is whatever
		// the client reports, so this cannot say which of the two it was.
		b.WriteString("\nBoth name one pid: that process moved to a new port, or another process quoted its pid.")
		return b.String()
	}
	b.WriteString("\nThe evicted instance is still running and will receive no traffic.")
	return b.String()
}
