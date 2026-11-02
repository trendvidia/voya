package dns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// transports is both of them, for the tests that hold on each: the server
// answers over UDP and TCP (#184), and a rule that held on one and not the
// other is exactly what one table across both catches.
var transports = []string{"udp", "tcp"}

// serve starts the real server on an ephemeral loopback port, on both
// transports, and returns its address and the server for tests that need to
// close it themselves. Everything else is cleaned up.
func serve(t *testing.T) (string, *Server) {
	t.Helper()
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := NewServer(ln, func(string, ...any) {})

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String(), srv
}

// resolver queries addr over one transport, which is how a consumer reaches
// the server once /etc/resolver/internal points here — over UDP for
// getaddrinfo and friends, over TCP for a resolver forwarding a TCP client.
func resolver(addr, network string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

func TestEveryInternalNameResolvesToLoopback(t *testing.T) {
	addr, _ := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Names nobody registered included on purpose: the wildcard answers
	// every one, and the proxy is what explains an unknown service (ADR 002).
	for _, network := range transports {
		resolver := resolver(addr, network)
		for _, name := range []string{"payments.internal", "billing.internal", "never-registered.internal"} {
			t.Run(network+"/"+name, func(t *testing.T) {
				addrs, err := resolver.LookupHost(ctx, name)
				if err != nil {
					t.Fatalf("LookupHost(%q): %v", name, err)
				}
				if len(addrs) == 0 {
					t.Fatalf("LookupHost(%q) returned nothing", name)
				}
				for _, addr := range addrs {
					if addr != "127.0.0.1" {
						t.Errorf("LookupHost(%q) = %v, want 127.0.0.1", name, addrs)
					}
				}
			})
		}
	}
}

// TestDigOverTCP asks a real client rather than this package's own encoder:
// `dig +tcp` frames the query with RFC 1035 §4.2.2's length prefix, sends an
// EDNS OPT record the server ignores, and refuses a reply whose framing is
// off — which is the whole of what #184 is about, and what a Go resolver
// written beside the server would agree with by construction.
func TestDigOverTCP(t *testing.T) {
	dig, err := exec.LookPath("dig")
	if err != nil {
		t.Skip("dig is not installed here; the framing is asserted by the Go resolver in TestEveryInternalNameResolvesToLoopback")
	}
	addr, _ := serve(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}

	for _, tc := range []struct{ flag, name, want string }{
		{"+tcp", "payments.internal", "127.0.0.1"},
		{"+notcp", "payments.internal", "127.0.0.1"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, dig, tc.flag, "+short", "+tries=1", "+time=5", "-p", port, "@"+host, tc.name, "A")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", cmd, err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("%s answered %q, want %q", cmd, got, tc.want)
			}
		})
	}
}

// TestAAAAExistsWithNoAddress: a resolver that asks for AAAA first must be
// told the name exists and has no v6 address, so it falls back to A.
// NXDOMAIN here would fail the name outright on those clients.
func TestAAAAExistsWithNoAddress(t *testing.T) {
	addr, _ := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, network := range transports {
		t.Run(network, func(t *testing.T) {
			resolver := resolver(addr, network)
			addrs, err := resolver.LookupIP(ctx, "ip6", "payments.internal")
			if err != nil {
				// "no such host" for the v6 lookup specifically is fine —
				// the name has no AAAA. What must not happen is the name
				// failing outright, which the v4 lookup below checks.
				var dnsErr *net.DNSError
				if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
					t.Fatalf("LookupIP(ip6): %v", err)
				}
			}
			if len(addrs) != 0 {
				t.Errorf("LookupIP(ip6) = %v, want none", addrs)
			}

			v4, err := resolver.LookupIP(ctx, "ip4", "payments.internal")
			if err != nil {
				t.Fatalf("the v4 lookup failed after a v6 miss: %v", err)
			}
			if len(v4) == 0 || !v4[0].Equal(Loopback) {
				t.Errorf("LookupIP(ip4) = %v, want 127.0.0.1", v4)
			}
		})
	}
}

// TestNamesThatAreNotOurs are refused rather than denied: this server has no
// standing to claim a name does not exist anywhere on the internet.
func TestNamesThatAreNotOurs(t *testing.T) {
	addr, _ := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// "localhost" is deliberately absent: Go's resolver answers it without
	// asking any server, so it would test the standard library rather than
	// this one.
	for _, network := range transports {
		resolver := resolver(addr, network)
		for _, name := range []string{"example.com", "payments.local", "api.example.internal.co"} {
			t.Run(network+"/"+name, func(t *testing.T) {
				if addrs, err := resolver.LookupHost(ctx, name); err == nil {
					t.Errorf("LookupHost(%q) = %v, want a failure — voya answers only %s", name, addrs, registrySuffix)
				}
			})
		}
	}
}

const registrySuffix = ".internal"

func TestMalformedQueriesDoNotCrashTheServer(t *testing.T) {
	addr, _ := serve(t)

	payloads := [][]byte{
		{},               // empty
		{0x00},           // shorter than a header
		make([]byte, 12), // header with no question
		{0xAB, 0xCD, 0x01, 0x00, 0x00, 0x01, // truncated mid-question
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x08, 'p', 'a'},
	}
	for _, network := range transports {
		t.Run(network, func(t *testing.T) {
			for _, payload := range payloads {
				client, err := net.Dial(network, addr)
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				// On TCP the payload is framed as its own length says, so
				// the server reads exactly the malformed thing; and a
				// separate case below lies about the length instead.
				if _, err := client.Write(frame(network, payload)); err != nil {
					t.Fatalf("Write: %v", err)
				}
				_ = client.Close()
			}
			if network == "tcp" {
				// A length prefix promising more than ever arrives, then
				// the client hangs up: the server must not be stuck on it.
				client, err := net.Dial(network, addr)
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				if _, err := client.Write([]byte{0xFF, 0xFF, 0x01}); err != nil {
					t.Fatalf("Write: %v", err)
				}
				_ = client.Close()
			}

			// The server must still answer a good query afterwards.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := resolver(addr, network).LookupHost(ctx, "payments.internal"); err != nil {
				t.Errorf("the server stopped answering over %s after malformed input: %v", network, err)
			}
		})
	}
}

// TestEveryQueryGetsAnAnswer covers the messages voya will not serve. A
// resolver told nothing waits out its own timeout and reports only that,
// which is the hang ADR 005 exists to prevent — so each of these gets an
// RCODE naming the reason instead of silence.
//
// One table, both transports: the answering is one function, and this is
// what keeps a rule from being added to UDP and not TCP (#184).
func TestEveryQueryGetsAnAnswer(t *testing.T) {
	addr, _ := serve(t)

	for _, tc := range []struct {
		name    string
		query   []byte
		rcode   byte
		answers int
	}{
		{"an ordinary A query", query("payments.internal", typeA, classIN), 0, 1},
		{"an AAAA query", query("payments.internal", typeAAAA, classIN), 0, 0},      // NOERROR, no answer
		{"a name that is not ours", query("example.com", typeA, classIN), 5, 0},     // Refused
		{"a class voya has nothing in", query("payments.internal", typeA, 3), 4, 0}, // Not implemented
		{"the mDNS unicast-response bit", query("payments.internal", typeA, 0x8001), 4, 0},
		{"a question that stops mid-label", []byte{
			0xAB, 0xCD, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x08, 'p', 'a'}, 1, 0}, // Format error
		{"no question at all", make([]byte, headerLen), 1, 0},
	} {
		for _, network := range transports {
			t.Run(network+"/"+tc.name, func(t *testing.T) {
				reply := exchange(t, network, addr, tc.query)
				if len(reply) < headerLen {
					t.Fatalf("reply is %d bytes, shorter than a header", len(reply))
				}
				if reply[2]&0x80 == 0 {
					t.Errorf("reply does not have QR set: %x", reply[:4])
				}
				if reply[0] != tc.query[0] || reply[1] != tc.query[1] {
					t.Errorf("reply id is %x, want the query's %x", reply[:2], tc.query[:2])
				}
				if got := reply[3] & 0x0F; got != tc.rcode {
					t.Errorf("rcode is %d, want %d", got, tc.rcode)
				}
				if got := int(reply[6])<<8 | int(reply[7]); got != tc.answers {
					t.Errorf("ANCOUNT is %d, want %d", got, tc.answers)
				}
				if tc.answers == 1 && !bytes.HasSuffix(reply, Loopback.To4()) {
					t.Errorf("the answer does not end in 127.0.0.1: %x", reply)
				}
			})
		}
	}
}

// TestAResponseIsNeverAnswered: replying to a reply is how two servers
// pointed at each other turn one stray packet into a loop.
func TestAResponseIsNeverAnswered(t *testing.T) {
	addr, _ := serve(t)

	response := query("payments.internal", typeA, classIN)
	response[2] |= 0x80 // QR: this is an answer, not a question

	for _, network := range transports {
		t.Run(network, func(t *testing.T) {
			client, err := net.Dial(network, addr)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer client.Close()
			if _, err := client.Write(frame(network, response)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := client.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
				t.Fatalf("SetReadDeadline: %v", err)
			}
			buf := make([]byte, maxUDPMessage)
			// On TCP the server hangs up on a response, so EOF is the right
			// outcome there; on UDP the read times out. Either way, no bytes.
			if n, err := client.Read(buf); err == nil {
				t.Errorf("the server answered a response with %d bytes: %x", n, buf[:n])
			}
		})
	}
}

// TestALongNameIsTruncatedOnUDPAndAnsweredOnTCP: a name long enough to
// overflow 512 bytes is truncated on UDP rather than sent oversized, which
// resolvers drop without explanation — and TC means what it says, because
// the same server answers the retry over TCP whole (#184).
func TestALongNameIsTruncatedOnUDPAndAnsweredOnTCP(t *testing.T) {
	addr, _ := serve(t)
	long := strings.Repeat("verylonglabelindeed.", 12) + "internal" // ~250 bytes on the wire
	q := query(long, typeA, classIN)

	udp := exchange(t, "udp", addr, q)
	if len(udp) > maxUDPMessage {
		t.Errorf("the UDP reply is %d bytes, over the %d-byte limit", len(udp), maxUDPMessage)
	}
	if udp[2]&0x02 == 0 {
		t.Errorf("the UDP reply is not marked truncated: %x", udp[:4])
	}
	if ancount := int(udp[6])<<8 | int(udp[7]); ancount != 0 {
		t.Errorf("a truncated reply carries %d answers, want none", ancount)
	}

	tcp := exchange(t, "tcp", addr, q)
	if len(tcp) <= maxUDPMessage {
		t.Errorf("the TCP reply is %d bytes: this name was chosen to need more than %d, so the test is not testing truncation", len(tcp), maxUDPMessage)
	}
	if tcp[2]&0x02 != 0 {
		t.Errorf("the TCP reply is marked truncated: %x", tcp[:4])
	}
	if ancount := int(tcp[6])<<8 | int(tcp[7]); ancount != 1 {
		t.Errorf("the TCP reply carries %d answers, want the one A record", ancount)
	}
	if !bytes.HasSuffix(tcp, Loopback.To4()) {
		t.Errorf("the TCP answer does not end in 127.0.0.1: %x", tcp)
	}
}

// TestTheLongestLegalNameIsAnsweredWholeOnTCP pins what [maxTCPMessage]'s
// comment claims: no reply this server builds can reach 65535, so the
// truncation branch is dead on TCP. That holds only because [parse] rejects
// a name over [maxNameLen] — without the limit a TCP frame carries a 32 KB
// name whose reply comes back with TC set on the transport the client is
// already using — so both halves are checked here, the longest name that is
// legal and the shortest that is not.
func TestTheLongestLegalNameIsAnsweredWholeOnTCP(t *testing.T) {
	addr, _ := serve(t)

	// 12 × "verylonglabelindeed" (20 bytes each with its length byte), then
	// a 4-byte label, "internal" and the root: 255 on the wire exactly.
	longest := strings.Repeat("verylonglabelindeed.", 12) + "abcd.internal"
	q := query(longest, typeA, classIN)
	if got := len(q) - headerLen - 4; got != maxNameLen {
		t.Fatalf("the name is %d bytes on the wire, want exactly %d — the test is not testing the limit", got, maxNameLen)
	}

	reply := exchange(t, "tcp", addr, q)
	if reply[2]&0x02 != 0 {
		t.Errorf("the longest legal name came back truncated on TCP: %x", reply[:4])
	}
	if ancount := int(reply[6])<<8 | int(reply[7]); ancount != 1 {
		t.Errorf("the reply carries %d answers, want the one A record", ancount)
	}
	if len(reply) >= maxTCPMessage {
		t.Errorf("the largest reply is %d bytes, at or over the %d the framing allows", len(reply), maxTCPMessage)
	}

	// One label longer, and the name is malformed: FORMERR on both
	// transports, which is what keeps the branch above dead.
	overlong := "x." + longest
	for _, network := range transports {
		t.Run(network+"/one byte over the limit", func(t *testing.T) {
			reply := exchange(t, network, addr, query(overlong, typeA, classIN))
			if got := reply[3] & 0x0F; got != 1 {
				t.Errorf("rcode is %d, want 1 (Format error)", got)
			}
			if reply[2]&0x02 != 0 {
				t.Errorf("a name too long to parse came back truncated: %x", reply[:4])
			}
		})
	}
}

// TestAnAcceptShortageDoesNotStopTheServer: running out of file descriptors
// is a condition that clears, and [Server.Serve] stops both transports on
// the first error — so an EMFILE from Accept must be waited out rather than
// taking UDP, where every ordinary lookup arrives, down with it. Anything
// else from Accept is still fatal, which the second half checks.
func TestAnAcceptShortageDoesNotStopTheServer(t *testing.T) {
	t.Run("EMFILE is waited out", func(t *testing.T) {
		var logs logRecorder
		addr, served := serveWithAcceptErrors(t, &logs, syscall.EMFILE, syscall.EMFILE)

		for _, network := range transports {
			reply := exchange(t, network, addr, query("payments.internal", typeA, classIN))
			if ancount := int(reply[6])<<8 | int(reply[7]); ancount != 1 {
				t.Errorf("over %s the server answered %d records after the shortage, want 1", network, ancount)
			}
		}
		select {
		case err := <-served:
			t.Fatalf("Serve returned %v on an EMFILE that had already cleared", err)
		default:
		}
		if got := logs.String(); !strings.Contains(got, "retrying in") {
			t.Errorf("the shortage was not logged as retried: %q", got)
		}
	})

	t.Run("anything else is fatal", func(t *testing.T) {
		var logs logRecorder
		_, served := serveWithAcceptErrors(t, &logs, syscall.EINVAL)

		select {
		case err := <-served:
			if err == nil || !strings.Contains(err.Error(), "accept") {
				t.Fatalf("Serve returned %v, want an accept failure", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve is still running 5s after a fatal Accept error")
		}
	})
}

// serveWithAcceptErrors starts a server whose TCP Accept fails with errs,
// one per call, before it starts accepting for real. It returns the address
// and the channel Serve's result arrives on; the caller decides whether a
// result is expected, so nothing here fails the test on its own.
func serveWithAcceptErrors(t *testing.T, logs *logRecorder, errs ...error) (string, <-chan error) {
	t.Helper()
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.tcp = &flakyListener{Listener: ln.tcp, errs: errs}
	srv := NewServer(ln, logs.logf)

	served := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		served <- srv.Serve()
		close(done)
	}()
	// done rather than served: the caller may have taken the result
	// already, and a cleanup waiting for a second one would hang.
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return ln.Addr().String(), served
}

// flakyListener hands out queued errors from Accept before delegating to the
// real listener, which is how a descriptor shortage is reproduced without
// exhausting the test process's own descriptors.
type flakyListener struct {
	net.Listener
	mu   sync.Mutex
	errs []error
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

// logRecorder collects what the server logged, for a test asserting on it
// from its own goroutine rather than from the server's.
type logRecorder struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (r *logRecorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(&r.buf, format+"\n", args...)
}

func (r *logRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// TestCloseEndsAnOpenTCPConnection: a client holding a connection with no
// query on it must not hold the server up. Close ends the connection, and
// Serve returns only once the goroutine that was serving it has — which is
// what the daemon's Close waits on, so a lingering client cannot make it
// burn the shutdown grace.
func TestCloseEndsAnOpenTCPConnection(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := NewServer(ln, func(string, ...any) {})
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	// One answered query first, so the connection is one the server is
	// serving rather than one it has not accepted yet.
	if _, err := client.Write(frame("tcp", query("payments.internal", typeA, classIN))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := readFramed(client, 5*time.Second); err != nil {
		t.Fatalf("no reply on the connection before Close: %v", err)
	}

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Serve has not returned %s after Close, with a TCP client still connected", time.Since(start))
	}
	// Well inside the idle timeout, or Close did nothing and the idle
	// timeout is what ended it.
	if took := time.Since(start); took >= tcpIdleTimeout {
		t.Errorf("Serve took %s to return after Close, which is the idle timeout doing Close's job", took)
	}
	if _, err := readFramed(client, 5*time.Second); err == nil {
		t.Error("the client's connection was not closed by Close")
	}
}

// wsaeconnreset is Windows' connection-reset errno, WSAECONNRESET (10054),
// named by its number for the reason internal/diag/refused.go gives for
// WSAECONNREFUSED: [syscall.ECONNRESET] on Windows is a synthetic value no
// socket returns, so errors.Is against it alone is false there for every
// real reset. No unix errno reaches five digits.
const wsaeconnreset = syscall.Errno(10054)

// closedByPeer reports whether err is the server having closed a connection
// under the client: EOF when the client had nothing in flight, a reset when
// it had (a close with the query still unread resets), on every OS voya
// builds for.
func closedByPeer(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, wsaeconnreset)
}

// TestTCPConnectionsAreCapped: past [maxTCPConns] open connections the
// server closes the next one unserved rather than give it a goroutine, a
// descriptor and a 64 KB buffer (#195) — while the ones inside the cap go on
// being answered, and a slot freed by a client hanging up is a slot the next
// client gets. The cap is lowered for the test; what is under test is the
// count against it, not the number 128.
func TestTCPConnectionsAreCapped(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	logs := &logRecorder{}
	srv := NewServer(ln, logs.logf)
	const capacity = 3
	srv.maxConns = capacity
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	q := frame("tcp", query("payments.internal", typeA, classIN))

	// Fill the cap. A reply on each proves the server is serving it, so it
	// is counted — a Dial alone returns once the kernel has queued the
	// connection, before Accept has seen it.
	inside := make([]net.Conn, 0, capacity)
	for i := range capacity {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial %d: %v", i, err)
		}
		defer conn.Close()
		if _, err := conn.Write(q); err != nil {
			t.Fatalf("Write on connection %d: %v", i, err)
		}
		if _, err := readFramed(conn, 5*time.Second); err != nil {
			t.Fatalf("connection %d, inside the cap, was not answered: %v", i, err)
		}
		inside = append(inside, conn)
	}

	// The cap plus one: closed, and closed rather than left to the idle
	// timeout — EOF or a reset (the query it wrote is unread when the
	// server closes, and a close with unread data resets), never a deadline.
	over, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial past the cap: %v", err)
	}
	defer over.Close()
	if _, err := over.Write(q); err != nil {
		t.Fatalf("Write past the cap: %v", err)
	}
	if reply, err := readFramed(over, 5*time.Second); !closedByPeer(err) {
		t.Fatalf("connection %d with a cap of %d: reply %x, err %v; want it closed (EOF or a reset)", capacity+1, capacity, reply, err)
	}
	if !strings.Contains(logs.String(), "refusing") {
		t.Errorf("a refused connection left no trace in the log:\n%s", logs.String())
	}

	// A second refusal with nothing accepted in between says nothing more:
	// the episode is logged once, not once per try. track writes the line
	// before the caller closes the connection, so observing this one closed
	// is enough to know whether a line came with it.
	againOver, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("second Dial past the cap: %v", err)
	}
	defer againOver.Close()
	if _, err := againOver.Write(q); err != nil {
		t.Fatalf("second Write past the cap: %v", err)
	}
	if reply, err := readFramed(againOver, 5*time.Second); !closedByPeer(err) {
		t.Fatalf("the second connection past the cap: reply %x, err %v; want it closed (EOF or a reset)", reply, err)
	}
	if n := strings.Count(logs.String(), "refusing"); n != 1 {
		t.Errorf("%d refusal lines for two refusals in one episode, want 1:\n%s", n, logs.String())
	}

	// The refusal cost the connections inside the cap nothing.
	if _, err := inside[0].Write(q); err != nil {
		t.Fatalf("Write on the first connection after a refusal: %v", err)
	}
	if _, err := readFramed(inside[0], 5*time.Second); err != nil {
		t.Errorf("the first connection, inside the cap, went unanswered after another was refused: %v", err)
	}

	// A client hanging up frees its slot for the next one. The server
	// notices the hang-up on its own goroutine, so wait for the count
	// rather than race it.
	_ = inside[1].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		open := len(srv.conns)
		srv.mu.Unlock()
		if open < capacity {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections still counted %s after a client closed one", open, 5*time.Second)
		}
		time.Sleep(time.Millisecond)
	}
	next, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial after a slot freed: %v", err)
	}
	defer next.Close()
	if _, err := next.Write(q); err != nil {
		t.Fatalf("Write after a slot freed: %v", err)
	}
	if _, err := readFramed(next, 5*time.Second); err != nil {
		t.Errorf("a connection into a freed slot was not answered: %v", err)
	}
}

// TestARefusalDoesNotLogUnderTheLock: in the daemon logf is a synchronous
// write to stderr, so a refusal written under s.mu would put a log consumer
// that has stopped reading between every connection and its untrack, and
// Close behind them (#195). With a refusal parked inside logf, a client
// hanging up must still free its slot and Close must still return.
func TestARefusalDoesNotLogUnderTheLock(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := NewServer(ln, func(format string, args ...any) {
		if strings.Contains(format, "refusing") {
			once.Do(func() { close(blocked) })
			<-release
		}
	})
	srv.maxConns = 1
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	// Runs before the cleanup above, so Close is never the thing waiting on
	// a logf this test parked.
	t.Cleanup(func() { close(release) })
	q := frame("tcp", query("payments.internal", typeA, classIN))

	only, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer only.Close()
	if _, err := only.Write(q); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := readFramed(only, 5*time.Second); err != nil {
		t.Fatalf("the only connection, inside the cap, was not answered: %v", err)
	}
	over, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial past the cap: %v", err)
	}
	defer over.Close()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("no refusal reached logf 5s after a connection past the cap")
	}

	// Wait for the hang-up to free the slot without ever blocking on s.mu:
	// under the defect this test exists for the parked log write holds it,
	// and a test that waited there would hang rather than fail.
	_ = only.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		open := -1
		if srv.mu.TryLock() {
			open = len(srv.conns)
			srv.mu.Unlock()
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with a refusal parked in logf, %d connections still counted (-1: s.mu is held) %s after the only client hung up", open, 5*time.Second)
		}
		time.Sleep(time.Millisecond)
	}
	// And shutdown does not queue behind the log write either. (The accept
	// loop does: it is the goroutine parked in logf. What must not stall is
	// everything that only wanted the lock.)
	closed := make(chan error, 1)
	go func() { closed <- srv.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Close is still waiting %s after a refusal parked in logf", 5*time.Second)
	}
}

// TestBothTransportsBindLoopback: Listen refuses a non-loopback address
// before binding anything, and what it then binds is loopback on both
// transports — asserted on the sockets, since the refusal test alone would
// pass a TCP bind that ignored the address it was given.
func TestBothTransportsBindLoopback(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	addrs := ln.Addrs()
	if len(addrs) != 2 {
		t.Fatalf("Addrs() = %v, want one per transport", addrs)
	}
	var port string
	for _, addr := range addrs {
		host, p, err := net.SplitHostPort(addr.String())
		if err != nil {
			t.Fatalf("SplitHostPort(%q): %v", addr, err)
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			t.Errorf("%s bound %s, want loopback", addr.Network(), addr)
		}
		if port == "" {
			port = p
		} else if p != port {
			t.Errorf("the transports bound different ports: %v", addrs)
		}
	}
	if !slices.Equal([]string{addrs[0].Network(), addrs[1].Network()}, transports) {
		t.Errorf("Addrs() networks are %s and %s, want udp and tcp", addrs[0].Network(), addrs[1].Network())
	}
}

// TestListenRefusesATCPPortThatIsTaken: one number, both transports, or
// nothing — a server answering on UDP and dead on TCP is the failure #184
// removed, so a TCP port held by someone else stops Listen outright.
func TestListenRefusesATCPPortThatIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holding a TCP port: %v", err)
	}
	defer taken.Close()

	ln, err := Listen(context.Background(), taken.Addr().String())
	if err == nil {
		_ = ln.Close()
		t.Fatalf("Listen bound %s with its TCP side already held", taken.Addr())
	}
	if !strings.Contains(err.Error(), "tcp") {
		t.Errorf("the error does not say which transport was taken: %v", err)
	}
}

// query builds a wire-format DNS query for name.
func query(name string, qtype, qclass uint16) []byte {
	msg := []byte{0xAB, 0xCD, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for _, label := range strings.Split(name, ".") {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0)
	return append(msg, byte(qtype>>8), byte(qtype), byte(qclass>>8), byte(qclass))
}

// exchange sends one query over network and returns the reply, unframed,
// failing if none arrives.
func exchange(t *testing.T, network, addr string, q []byte) []byte {
	t.Helper()
	client, err := net.Dial(network, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Write(frame(network, q)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if network == "tcp" {
		reply, err := readFramed(client, 5*time.Second)
		if err != nil {
			t.Fatalf("no reply: %v", err)
		}
		return reply
	}
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, maxUDPMessage)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	return buf[:n]
}

// frame is msg as network carries it: bare on UDP, behind RFC 1035 §4.2.2's
// two-byte length on TCP.
func frame(network string, msg []byte) []byte {
	if network != "tcp" {
		return msg
	}
	return append([]byte{byte(len(msg) >> 8), byte(len(msg))}, msg...)
}

// readFramed reads one length-prefixed message from a TCP connection.
func readFramed(conn net.Conn, within time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return nil, err
	}
	var prefix [2]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, int(prefix[0])<<8|int(prefix[1]))
	if _, err := io.ReadFull(conn, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func TestListenRefusesAnythingButLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.10:0", ":0"} {
		t.Run(addr, func(t *testing.T) {
			conn, err := Listen(context.Background(), addr)
			if err == nil {
				_ = conn.Close()
				t.Fatalf("Listen(%q) bound a non-loopback address", addr)
			}
			if !strings.Contains(err.Error(), "loopback") {
				t.Errorf("the error does not say why: %v", err)
			}
		})
	}
}
