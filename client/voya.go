// Package voya is what a service imports to take part in local development.
//
// One call in main.go does both halves:
//
//	ln, err := net.Listen("tcp", "127.0.0.1:0") // the OS picks the port
//	if err != nil {
//		return err
//	}
//	stop, err := voya.Join(ctx, "payments", ln.Addr())
//	if err != nil {
//		return err
//	}
//	defer stop()
//
// After that, this service is reachable at http://payments.internal:9777,
// and its own calls to http://billing.internal:9777 reach billing — the same
// dial strings it will use on Fly, with no build tags and no environment
// branch in application code.
//
// # Stopping
//
// Deregistering and draining are two different things, and they go in this
// order:
//
//	stop()                 // leave the registry, and wait for voya to notice
//	srv.Shutdown(drainCtx) // finish the requests already accepted
//
// stop blocks for [DefaultShutdownGrace] on the way out, because voya learns
// a service is gone a moment after its stream closes and keeps routing to it
// until then. Closing the listener inside that window is what produces the
// connection resets a developer reads as flakiness in their own service.
//
// A bare `defer stop()` gets this right only while nothing is deferred after
// it: defers run last-registered-first, so a `defer srv.Shutdown(...)` written
// below would run first and close the listener while voya was still sending
// requests to it.
//
// # Dev-only scaffolding
//
// Every exported call here is a no-op when FLY_APP_NAME is set. In
// production the platform provides all of this: each Machine has its own 6PN
// address, DNS resolves *.internal, and there is nothing to register with.
// ADR 004 calls the registration hook the one piece of this design with no
// production counterpart, and asks that it look like it — hence a single
// call that quietly does nothing there.
//
// FLY_APP_NAME is the only switch, and voya cannot tell a production host from
// a developer's machine any other way. On a production host that is not Fly, set
// it to any value, or do not call [Join]. Otherwise a *.internal call on the
// proxy port goes to loopback, and whatever is listening there receives it,
// bearer tokens included.
//
// # Dependencies
//
// The package's own imports are deliberately thin: connect-go, the generated
// registration schema, and the standard library. Every service in the fleet
// imports this package, so anything it imports is something they all import
// — and the module boundary enforces it (#36): client/ is its own module,
// whose go.mod requires connect-go, the generated schemas and protobuf, and
// nothing of the daemon's.
package voya

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// Fleet convention, fixed by ADR 001: every service declares 9777 in its
// fly.toml, which is what makes http://payments.internal:9777 identical as a
// dial string in both environments. Override only if voya itself was
// reconfigured.
const (
	DefaultProxyPort        = 9777
	DefaultRegistrationPort = 9779

	// Suffix is the TLD voya routes. Kept here rather than imported from
	// internal/registry so this package stays importable on its own.
	Suffix = ".internal"

	// FlyAppEnv is how a process knows it is on Fly. Set by the platform on
	// every Machine.
	FlyAppEnv = "FLY_APP_NAME"
)

// Backoff bounds, applied when the registration stream cannot be opened or
// breaks. Bounded and jittered so that a service running with no voya at all
// costs one connection attempt every half minute rather than a spin.
const (
	DefaultMinBackoff = 250 * time.Millisecond
	DefaultMaxBackoff = 30 * time.Second
)

// Reclaim windows, applied after this instance has been deposed (#27).
//
// The window is the judgement call. It has to be long enough for the workflow
// it exists for — replace a service to try a fix, find the replacement worse,
// kill it, and have the original resume — and short enough that a process
// forgotten at the end of the day does not take a name back the next morning
// and serve from a build nobody remembers starting.
//
// Five minutes covers "I killed it, give me the old one back" comfortably and
// expires long before anybody walks away from their desk. Nobody has measured
// it, and there is nothing here to measure: it is a bound on a developer's
// patience, not on a race. That is what separates it from
// [DefaultShutdownGrace], which was a guess for the same reason until #7 found
// the quantity underneath it.
const (
	DefaultReclaimWindow   = 5 * time.Minute
	DefaultReclaimInterval = 15 * time.Second
)

// DefaultShutdownGrace is how long [Join]'s stop function keeps a service
// routable after tearing its registration down.
//
// Closing the stream is the deregistration, and voya acts on it almost at
// once — but not instantly, and for that moment it is still routing requests
// to a service that believes it has left. A stop that returns inside that
// window hands control back to a service whose next move is to close its
// listener, and the requests voya routed meanwhile arrive at nothing. What
// the developer sees is intermittent connection resets on ordinary restarts,
// which reads as flakiness in their own service rather than as a race in the
// tool.
//
// Unlike the 250ms ADR 004 started from, this one is measured (#7). Eviction
// propagation — from the stream closing to the registry no longer handing this
// service out, which is where the proxy decides, once per request — was sampled
// over 200 restarts per condition through the real client, a real daemon and a
// real proxy:
//
//	machine   voya      p50    p90     p99     max
//	idle      idle      142µs  167µs   216µs   246µs
//	idle      proxying  229µs  450µs   1.0ms   1.1ms
//	loaded    idle      250µs  700µs   6.0ms   15.8ms
//	loaded    proxying  530µs   11ms    28ms   32.8ms
//	CI host   proxying  5.2ms  28.7ms  66.8ms  106.7ms
//
// "loaded" is 32 spinning processes on a 16-core machine, which is harsher
// than the parallel compile a restart actually competes with. 50ms clears the
// worst sample of every one of those and costs a fifth of the guess.
//
// It does not clear the last row, which is a build host running three CI jobs
// at once, and that is deliberate: voya runs on the machine a developer is
// working at (ADR 001), and sizing this for a build host would tax every
// restart on every laptop for a machine class the tool does not target. What
// that row shows is the shape of the limit — any fixed grace is a bet on how
// contended the machine is. ADR 004's "Open: replacing the grace with an
// acknowledgement" is what stops betting; it needs a protocol change and a
// decision, and neither has been made (issue #53). What the
// table does not carry is the last hop: a request that read the registry a
// moment before the eviction is still on its way to the old port. That term is
// bounded by one handler dispatch, and two orders of magnitude of headroom over
// the worst sample above is what covers it.
//
// TestEvictionPropagationStaysInsideTheGrace, in test/acceptance, keeps the
// measurement honest as the code changes. Named rather than linked: it lives in
// a package that imports this one, so no doc link can reach it, and a link that
// resolves to nothing renders as literal brackets rather than failing.
//
// It does not cover requests already accepted. Those are the service's own
// http.Server.Shutdown to drain, and how long they need is a property of its
// handlers rather than anything voya can measure.
const DefaultShutdownGrace = 50 * time.Millisecond

// Eviction is delivered when another instance of this service takes its name
// (ADR 002).
type Eviction struct {
	// Reason is voya's rendering, naming both instances. Written to be read
	// by a human who is about to wonder why their build stopped receiving
	// traffic.
	Reason string

	// ReplacedByPort and ReplacedByPID identify the instance that took over.
	// The PID is zero if the incoming client did not supply one.
	ReplacedByPort uint32
	ReplacedByPID  int32
}

// Reclaim is delivered when this instance takes its name back after whoever
// deposed it went away.
type Reclaim struct {
	Service string

	// DeposedFor is how long this instance went without the name.
	DeposedFor time.Duration
}

// options is the resolved configuration for Join.
type options struct {
	proxyPort        uint32
	registrationPort uint32
	pid              int32
	minBackoff       time.Duration
	maxBackoff       time.Duration
	installDialer    bool
	reclaimWindow    time.Duration
	reclaimInterval  time.Duration
	shutdownGrace    time.Duration
	logf             func(string, ...any)
	onEvicted        func(Eviction)
	onReclaimed      func(Reclaim)
	flyAppName       func() string

	// flyApp is flyAppName's answer, read once in resolve. Every exported
	// call keys off this, including the dialer — which consults it per dial,
	// so it must not be an environment read.
	flyApp string
}

// onFly reports whether this process is running on Fly, where every exported
// call in this package is a no-op.
func (o *options) onFly() bool { return o.flyApp != "" }

// Option configures [Join], [Dialer] and [Transport].
type Option func(*options)

// WithProxyPort overrides the port the dialer sends *.internal traffic to.
//
// The dialer is its only reader for routing, so under [WithoutDialer] this
// option moves no traffic: [Join] accepts the port without erroring and
// installs nothing (#85). It is not the service telling voya where to serve
// — voya names the proxy port it actually bound in the registration
// acknowledgement, and the one thing the registration path does with this
// value is compare the two and warn when they differ (#116), since a dialer
// wired from the losing side of that disagreement sends the fleet's dial
// string to a port nothing answers.
//
// TestWithProxyPortIsInertUnderWithoutDialer holds the no-routing claim
// true; internal/registration's TestAMismatchedAcknowledgedProxyPortWarns
// holds the warning's, against the real daemon.
func WithProxyPort(port uint32) Option {
	return func(o *options) { o.proxyPort = port }
}

// WithRegistrationPort overrides the port the registration stream connects to.
func WithRegistrationPort(port uint32) Option {
	return func(o *options) { o.registrationPort = port }
}

// WithPID overrides the process id reported at registration. voya uses it
// only to name both instances when one evicts the other.
func WithPID(pid int32) Option {
	return func(o *options) { o.pid = pid }
}

// WithBackoff bounds the reconnect delay.
//
// A minimum of zero or less is raised to [DefaultMinBackoff] and a maximum
// below the minimum is raised to it, because the delay grows by doubling: a
// minimum of zero doubles to zero forever, which is not "retry eagerly" but
// a dial loop with no delay in it at all — the spin this package exists to
// avoid. There is no way to ask for one, deliberately.
func WithBackoff(minDelay, maxDelay time.Duration) Option {
	return func(o *options) { o.minBackoff, o.maxBackoff = minDelay, maxDelay }
}

// WithoutDialer registers this service without touching
// [http.DefaultTransport]. Use it when the service builds its own transports
// and installs [Dialer] itself.
func WithoutDialer() Option {
	return func(o *options) { o.installDialer = false }
}

// WithReclaim bounds how long, and how often, a deposed instance asks for its
// name back (#27). A zero window turns reclaiming off, which makes an
// eviction final.
//
// A zero or negative interval is raised to [DefaultReclaimInterval], for the
// reason [WithBackoff] gives: the wait is jittered, and a jittered zero is not
// "ask eagerly" but a reconnect loop with no delay in it at all, running for
// the whole window. Turning reclaiming off is what the window is for.
//
// Every attempt is conditional: it takes the name only if nothing holds it,
// so this can never depose whoever replaced this instance. That is what stops
// a retry loop becoming the slow version of the ping-pong it was written to
// prevent.
func WithReclaim(window, interval time.Duration) Option {
	return func(o *options) { o.reclaimWindow, o.reclaimInterval = window, interval }
}

// WithShutdownGrace overrides how long stop keeps this service routable after
// closing its registration stream — see [DefaultShutdownGrace] for what the
// wait is for and where the number came from.
//
// Zero or negative turns it off. That is a coherent request, and so it is
// honoured rather than clamped the way [WithBackoff] clamps: a service nothing
// calls — a one-shot job, a test fixture — has no in-flight traffic to protect
// and no reason to pay for the wait. A zero backoff, by contrast, asks for a
// dial loop with no delay in it, which is never what anybody wants.
func WithShutdownGrace(d time.Duration) Option {
	return func(o *options) { o.shutdownGrace = d }
}

// WithOnReclaimed is called when this instance takes its name back.
//
// The default says so prominently, and that is deliberate rather than noise:
// traffic has just gone back to a build somebody replaced on purpose, and
// nobody asked for it. An unannounced reclaim trades ADR 002's visible
// failure for a silent one.
func WithOnReclaimed(fn func(Reclaim)) Option {
	return func(o *options) { o.onReclaimed = fn }
}

// WithLogf replaces the destination for voya's own messages. These are few
// and all lifecycle: registered, lost voya, reconnected, evicted.
func WithLogf(logf func(string, ...any)) Option {
	return func(o *options) { o.logf = logf }
}

// WithOnEvicted is called when another instance takes this service's name.
//
// The default keeps the process running and says so loudly — the least
// destructive of the options, since a developer whose instance was deposed is
// usually mid-debug and losing the process would lose their work too. Exiting
// non-zero is the more honest signal and was rejected for exactly that reason
// (ADR 002, #27); this hook is where a different answer plugs in without
// touching this package.
//
// What the default will not do is re-register the ordinary way. ADR 004 has a
// broken stream reconnect, which applied to an eviction makes two live
// instances depose each other on a backoff timer forever. What follows an
// eviction instead is [WithReclaim]'s bounded series of conditional claims,
// every one of which loses to whoever holds the name.
func WithOnEvicted(fn func(Eviction)) Option {
	return func(o *options) { o.onEvicted = fn }
}

// withFlyAppName overrides how the package decides it is running on Fly.
// Unexported: production behaviour is not a knob, but tests must not depend
// on the ambient environment.
func withFlyAppName(fn func() string) Option {
	return func(o *options) { o.flyAppName = fn }
}

func resolve(opts []Option) *options {
	o := &options{
		proxyPort:        DefaultProxyPort,
		registrationPort: DefaultRegistrationPort,
		pid:              currentPID(),
		minBackoff:       DefaultMinBackoff,
		maxBackoff:       DefaultMaxBackoff,
		installDialer:    true,
		reclaimWindow:    DefaultReclaimWindow,
		reclaimInterval:  DefaultReclaimInterval,
		shutdownGrace:    DefaultShutdownGrace,
		logf:             log.Printf,
		flyAppName:       func() string { return os.Getenv(FlyAppEnv) },
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.onReclaimed == nil {
		o.onReclaimed = func(rc Reclaim) {
			o.logf("voya: %s is serving again after %s without it — if you replaced it because it was wrong, that build is back",
				rc.Service+Suffix, rc.DeposedFor.Round(time.Second))
		}
	}
	if o.onEvicted == nil {
		o.onEvicted = func(e Eviction) { o.logf("voya: %s", e.Reason) }
	}
	// Clamped rather than rejected: these are knobs on scaffolding, and
	// failing a service's startup over a backoff value would be a worse
	// answer than the bound. See [WithBackoff] for why zero is not a
	// legitimate request.
	if o.minBackoff <= 0 {
		o.minBackoff = DefaultMinBackoff
	}
	if o.maxBackoff < o.minBackoff {
		o.maxBackoff = o.minBackoff
	}
	// The same clamp, for the same reason, on the interval between claims: a
	// zero one spins for the length of the window. Conditioned on the window
	// so that WithReclaim(0, 0) still means "off" rather than "off, with a
	// default interval nothing will ever use".
	if o.reclaimWindow > 0 && o.reclaimInterval <= 0 {
		o.reclaimInterval = DefaultReclaimInterval
	}
	o.flyApp = o.flyAppName()
	return o
}

// OnFly reports whether this process is running on Fly, where every call in
// this package is a no-op.
func OnFly() bool { return os.Getenv(FlyAppEnv) != "" }

// Join makes this service reachable as <service>.internal and routes its own
// *.internal traffic through the local proxy.
//
// addr is the address the service actually bound — pass net.Listener.Addr()
// rather than what was asked for, since the point of binding port 0 is that
// the OS chooses (ADR 002).
//
// It does not block and does not fail when voya is not running: the
// registration stream is opened in the background and retried with bounded
// backoff, so a service starts and serves normally with no voya at all,
// degrading only in that nothing can reach it by name. The returned stop
// function ends the registration, which is what deregisters the service.
//
// stop blocks. It ends the stream, then holds for [DefaultShutdownGrace] so
// that voya has stopped routing here before the caller closes its listener —
// which is why it belongs *before* the service's own Shutdown rather than
// alongside it. The wait is skipped when there is nothing to wait for: a
// service that never got its registration onto the wire — nothing listening on
// the registration port — or one that was evicted, is not being routed to
// anyway.
//
// Sent, note, and not acknowledged. voya registers a service before it answers,
// so a stop landing between the two has to assume the route exists; the skip is
// for the cases where it demonstrably never did.
//
// Errors are reserved for things retrying cannot fix: an address that is not
// a TCP address, or a port of zero.
func Join(ctx context.Context, service string, addr net.Addr, opts ...Option) (stop func(), err error) {
	o := resolve(opts)

	if o.onFly() {
		// Production. Nothing to register with, and *.internal already
		// resolves. Deliberately silent past this one line: a service's
		// logs should not carry dev scaffolding.
		o.logf("voya: %s is set, so voya is inactive; %s resolves through ordinary DNS", FlyAppEnv, service+Suffix)
		return func() {}, nil
	}

	port, err := portOf(addr)
	if err != nil {
		return nil, err
	}

	if o.installDialer {
		installDialer(o)
	}

	runCtx, cancel := context.WithCancel(ctx)
	r := newRegistrar(service, port, o)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(runCtx)
	}()

	// sync.OnceFunc rather than a bool: a service with both `defer stop()`
	// and a stop from its signal handler can call this from two goroutines,
	// and a plain flag both races and lets the loser return while the
	// registration is still being torn down. OnceFunc blocks the second
	// caller until the first has finished, so stop returning always means
	// the name is out of the registry — and, after settle, that voya has had
	// long enough to act on it.
	return sync.OnceFunc(func() {
		cancel()
		<-done
		r.release()
		r.settle()
	}), nil
}

// maxPort is the highest port a listener can bind.
const maxPort = 65535

// currentPID reports this process id, or zero where it does not fit the
// field. The schema documents zero as "not supplied", and voya uses the pid
// only to name both instances in an eviction warning — so a platform with
// pids beyond int32 loses a nicety, not a registration.
func currentPID() int32 {
	pid := os.Getpid()
	if pid < math.MinInt32 || pid > math.MaxInt32 {
		return 0
	}
	return int32(pid)
}

// portOf extracts the bound port from a listener address.
func portOf(addr net.Addr) (uint32, error) {
	if addr == nil {
		return 0, fmt.Errorf("voya: no address: pass the listener's Addr() after binding")
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		// Anything else — a Unix socket, a test stub — is accepted only if
		// its String() carries a port the proxy could dial on loopback.
		_, portStr, err := net.SplitHostPort(addr.String())
		if err != nil {
			return 0, fmt.Errorf("voya: %q is not a TCP address the proxy can reach: %w", addr, err)
		}
		n, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			return 0, fmt.Errorf("voya: %q has no usable port: %w", addr, err)
		}
		// Held to the same rule as a *net.TCPAddr. Letting port 0 through
		// here would move the failure to the registry, which rejects it on
		// every reconnect for the life of the process — an error Join
		// promises to have raised itself.
		return checkedPort(int(n))
	}
	return checkedPort(tcp.Port)
}

// checkedPort rejects the ports that are not something a listener bound.
func checkedPort(port int) (uint32, error) {
	if port <= 0 || port > maxPort {
		return 0, fmt.Errorf("voya: port %d is not a bound port: register after Listen, using the listener's Addr()", port)
	}
	return uint32(port), nil
}
