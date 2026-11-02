// Package dns serves ADR 002's wildcard: every *.internal name resolves to
// 127.0.0.1, with no per-service records and no update path.
//
// This is not how voya's own services find each other. The client dialer is
// the primary resolution path (ADR 002 as amended), and it needs none of
// this. DNS exists for the consumers that are not our Go services — curl,
// grpcurl, Postman, browsers — and it is opt-in: it does nothing until
// someone points a resolver at it.
//
// A wildcard rather than per-service records, because every answer is the
// same constant. There is nothing to keep in sync, no update to lose, and an
// unknown name reaches the proxy — which can say "no service named paymnets,
// here is what is registered" instead of the resolver saying NXDOMAIN, which
// says nothing at all.
//
// The wire format is handled here rather than through a DNS library: what
// this server does is small and fixed, and the dependency would be larger
// than the code.
package dns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/trendvidia/voya/internal/registry"
)

// Loopback is the address every name resolves to.
var Loopback = net.IPv4(127, 0, 0, 1)

// TTL is what the answer claims, in seconds.
//
// Short, and deliberately: a resolver that caches this for an hour is a
// resolver that keeps answering after voya has stopped, which turns "voya is
// not running" into a connection refused with no explanation. The answer is
// a constant, so re-asking costs nothing.
const TTL = 30

// maxUDPMessage bounds a UDP DNS message. 512 bytes is the classic limit, and
// with one question, one answer and no extensions a reply reaches it only for
// a name several hundred bytes long — which [answer] truncates rather than
// exceed, pointing the client at TCP (#184).
const maxUDPMessage = 512

// maxTCPMessage bounds a DNS message on TCP, where RFC 1035 §4.2.2 frames
// each one with a two-byte length: 65535 is the largest that prefix can say.
// No reply this server builds comes near it: [parse] holds a name to
// [maxNameLen] and the reply carries it twice, once in the echoed question
// and once in the answer, so the largest reply possible is 540 bytes. Over
// TCP the truncation branch therefore never fires and a long name is
// answered whole — asserted by TestTheLongestLegalNameIsAnsweredWholeOnTCP,
// because a comment claiming a branch is dead is worth what its test is.
const maxTCPMessage = 1<<16 - 1

// maxNameLen is RFC 1035 §2.3.4's limit on a domain name: 255 bytes of wire
// format, length bytes and the root label included. [parse] enforces it, and
// not only for the sake of the spec — without it a TCP frame can carry a
// 32 KB name, whose reply passes 65535 and comes back with TC set on a
// transport the client is already using, which asks it to retry over TCP
// forever.
const maxNameLen = 255

// tcpIdleTimeout is how long a TCP client may hold a connection open with no
// query on it before the server closes it (RFC 7766 §6.2.3 says a server
// may, and should not have to wait forever). Every client voya has met sends
// its query the moment it connects, so this bounds only a stalled one.
const tcpIdleTimeout = 10 * time.Second

// maxTCPConns is how many TCP connections the server holds open at once;
// past it an accepted connection is closed unserved (#195). Each open one
// costs a goroutine, a descriptor and, once its length prefix arrives, a
// buffer of up to [maxTCPMessage] — for the whole of [tcpIdleTimeout] if the
// client then sends nothing — so without a cap N stalled local processes
// cost N × 64 KB and N descriptors with nothing to stop them. The listener is
// loopback-only, so the clients are this machine's resolvers, and every one
// voya has met opens a connection, asks, and closes: 128 is two orders of
// magnitude past that and still a bounded amount of memory. A refused
// client sees its connection closed with no answer on it, and that is a
// failed lookup rather than a graceful degrade: the resolver that came to
// TCP because a UDP reply had TC set (#184) cannot fall back to a transport
// that would truncate the same answer again. The cap is set where it is so
// that being refused is the overload case, not an ordinary one.
const maxTCPConns = 128

// listenAttempts is how many ephemeral ports [Listen] tries when asked for
// port 0: TCP and UDP are separate port spaces, so the number UDP was handed
// may already be taken on TCP, and the only remedy is to draw again.
const listenAttempts = 5

// Listener is voya's DNS port, bound on both transports RFC 7766 wants: UDP,
// which is where every ordinary lookup arrives, and TCP on the same address
// for the clients that speak it — dig +tcp, a local resolver forwarding a
// TCP client, or anything retrying a truncated UDP answer (#184).
type Listener struct {
	udp *net.UDPConn
	tcp net.Listener
}

// Addr is the bound address. Both transports share it: one host, one port.
func (l *Listener) Addr() net.Addr { return l.udp.LocalAddr() }

// Addrs is each transport's own view of the bound address, for the caller
// that wants to check what was actually bound rather than trust [Addr]'s
// claim that the two agree.
func (l *Listener) Addrs() []net.Addr { return []net.Addr{l.udp.LocalAddr(), l.tcp.Addr()} }

// Close closes both sockets.
func (l *Listener) Close() error {
	return errors.Join(l.udp.Close(), l.tcp.Close())
}

// Listen binds addr for the DNS server on UDP and TCP, refusing anything that
// is not loopback for the same reason the other two listeners do (ADR 002).
//
// One port, both transports: a client that gets TC on UDP retries the same
// server over TCP, and a resolver's configuration names one port. When addr
// asks for port 0 the kernel picks the UDP port and TCP is bound to match,
// drawing again if that number is taken on TCP.
func Listen(ctx context.Context, addr string) (*Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("dns: not binding %s: %w", addr, err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dns: %q is not a host:port address: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf(
			"dns: refusing to listen on %q: voya binds a loopback IP literal (127.0.0.1) "+
				"and nothing else — see ADR 002", addr)
	}

	attempts := 1
	if port == "0" {
		attempts = listenAttempts
	}
	var lc net.ListenConfig
	for {
		attempts--
		packet, err := lc.ListenPacket(ctx, "udp", addr)
		if err != nil {
			return nil, fmt.Errorf("dns: udp: %w", err)
		}
		udp, ok := packet.(*net.UDPConn)
		if !ok {
			_ = packet.Close()
			return nil, fmt.Errorf("dns: %q did not yield a UDP socket", addr)
		}
		tcp, err := lc.Listen(ctx, "tcp", udp.LocalAddr().String())
		if err == nil {
			return &Listener{udp: udp, tcp: tcp}, nil
		}
		_ = udp.Close()
		if attempts > 0 {
			// Whatever the reason, the remedy for an ephemeral draw is the
			// same — draw again — and matching the errno would only make
			// this a thing Windows spells differently.
			continue
		}
		return nil, fmt.Errorf("dns: tcp: %w", err)
	}
}

// Server answers *.internal queries on a [Listener], over both its
// transports.
type Server struct {
	ln   *Listener
	logf func(string, ...any)

	closed chan struct{}
	once   sync.Once

	// conns is every open TCP connection, so Close can end them and Serve
	// can wait for their goroutines rather than leave any behind.
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup

	// maxConns is [maxTCPConns], settable lower by a test that would
	// rather not open 129 sockets under the race detector to prove the cap.
	maxConns int
	// refusing is set once a connection has been refused for the cap and
	// cleared once one is accepted again, so a client that keeps
	// reconnecting against a full server costs one log line rather than one
	// per try. Any accept starts a fresh episode, so sustained churn at the
	// cap logs at the rate slots come free, not at the rate clients knock.
	refusing bool
}

// NewServer wraps a bound listener. logf may be nil.
func NewServer(ln *Listener, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{ln: ln, logf: logf, closed: make(chan struct{}), conns: make(map[net.Conn]struct{}), maxConns: maxTCPConns}
}

// Serve answers queries on both transports until Close is called. It returns
// nil on a clean close, so a caller can treat any other error as a real
// failure — and it returns only once every TCP connection it accepted has
// been closed and its goroutine has finished, so a caller that waited for
// Serve has waited for everything.
func (s *Server) Serve() error {
	errs := make(chan error, 2)
	go func() { errs <- s.serveUDP() }()
	go func() { errs <- s.serveTCP() }()

	// The first transport to fail takes the other down with it: a server
	// answering on one transport and dead on the other is the failure this
	// package exists to remove, so the whole thing stops and the caller
	// hears why.
	err := <-errs
	_ = s.Close()
	if second := <-errs; err == nil {
		err = second
	}
	s.wg.Wait()
	return err
}

// serveUDP answers datagrams, one reply per query, on this goroutine.
func (s *Server) serveUDP() error {
	buf := make([]byte, maxUDPMessage)
	for {
		n, from, err := s.ln.udp.ReadFromUDP(buf)
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("dns: udp: read: %w", err)
		}

		reply, ok := answer(buf[:n], maxUDPMessage)
		if !ok {
			// Unparseable enough that there is nothing to reply to — not
			// even an error, since a reply needs the query's ID.
			continue
		}
		if _, err := s.ln.udp.WriteToUDP(reply, from); err != nil {
			s.logf("voya: dns: replying to %s: %v", from, err)
		}
	}
}

// acceptBackoff is how long [Server.serveTCP] waits before accepting again
// after a descriptor shortage, doubling to maxAcceptBackoff while it lasts.
// The numbers are net/http's, whose Serve does the same thing for the same
// reason.
const (
	acceptBackoff    = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

// serveTCP accepts connections and hands each to [Server.serveConn] on its
// own goroutine, tracked so Close can end it — or, past [maxTCPConns] of
// them, closes the connection unserved and goes on accepting.
//
// An Accept that fails because the process is out of file descriptors is
// waited out rather than treated as fatal: [Server.Serve] stops both
// transports on the first error, so a momentary EMFILE would otherwise take
// down UDP — where every ordinary lookup arrives — and with it the daemon,
// for a condition that clears as soon as anything closes a descriptor. Go's
// own poller already retries EINTR, EAGAIN and ECONNABORTED before returning
// here, so what is left to handle is the shortage.
func (s *Server) serveTCP() error {
	backoff := time.Duration(0)
	for {
		conn, err := s.ln.tcp.Accept()
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
				if backoff = min(2*backoff, maxAcceptBackoff); backoff == 0 {
					backoff = acceptBackoff
				}
				s.logf("voya: dns: tcp: accept: %v; retrying in %s", err, backoff)
				select {
				case <-time.After(backoff):
					continue
				case <-s.closed:
					return nil
				}
			}
			return fmt.Errorf("dns: tcp: accept: %w", err)
		}
		backoff = 0
		switch s.track(conn) {
		case trackClosed:
			_ = conn.Close()
			return nil
		case trackFull:
			_ = conn.Close()
			continue
		case tracked:
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.serveConn(conn)
		}()
	}
}

// serveConn answers the queries on one TCP connection, in order, until the
// client closes it, it goes idle, or the server is closed. RFC 1035 §4.2.2's
// framing: a two-byte big-endian length before each message, both ways.
//
// The answering is [answer], the same function UDP uses — the transports
// differ in framing and in how large a reply may be, and in nothing else.
func (s *Server) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var prefix [2]byte
	for {
		if err := conn.SetReadDeadline(time.Now().Add(tcpIdleTimeout)); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			// EOF here is the client hanging up between queries, which is
			// the ordinary end of a TCP DNS conversation; anything else
			// ends it too, and there is nobody to tell.
			return
		}
		length := int(prefix[0])<<8 | int(prefix[1])
		query := make([]byte, length)
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}

		reply, ok := answer(query, maxTCPMessage)
		if !ok {
			// Nothing to reply to. On UDP that is a dropped datagram; on
			// TCP the connection is dropped with it, since a client that
			// sent a response, or no header at all, is not one this
			// conversation can continue with.
			return
		}
		if len(reply) > maxTCPMessage {
			// Unreachable for the replies this server builds (see
			// [maxTCPMessage]); kept so the prefix below can never lie.
			return
		}
		framed := make([]byte, 0, 2+len(reply))
		//nolint:gosec // G115: bounded by the maxTCPMessage check above, which gosec does not follow.
		framed = append(framed, byte(len(reply)>>8), byte(len(reply)))
		framed = append(framed, reply...)
		// Unreachable for the replies this server builds — the largest is
		// 540 bytes (see [maxTCPMessage]), which fits any send buffer, so
		// this Write does not block — and kept for the same reason the
		// length check above is: a socket that somehow will not take the
		// reply must not hold the goroutine and its descriptor forever.
		if err := conn.SetWriteDeadline(time.Now().Add(tcpIdleTimeout)); err != nil {
			return
		}
		if _, err := conn.Write(framed); err != nil {
			s.logf("voya: dns: replying to %s: %v", conn.RemoteAddr(), err)
			return
		}
	}
}

// A trackResult is what [Server.track] did with a connection.
type trackResult int

const (
	// tracked: recorded, and the caller serves it.
	tracked trackResult = iota
	// trackClosed: the server is closed, so the caller closes the connection
	// rather than serve it — Close has already been through the map and will
	// not see it.
	trackClosed
	// trackFull: [maxTCPConns] are already open, so the caller closes it
	// unserved and accepts the next.
	trackFull
)

// track records an accepted connection, or says why it did not. The count
// and the map are one thing under one lock, so a connection is never both
// refused for the cap and inside it.
//
// The refusal line is written after the lock is dropped, and before the
// caller closes the connection so a test can read it there. logf is a
// synchronous write to stderr in the daemon: held under s.mu, a consumer
// that has stopped draining the pipe would block every serveConn goroutine
// finishing in untrack, and Close behind them.
func (s *Server) track(conn net.Conn) trackResult {
	s.mu.Lock()
	res, open, refusal := tracked, 0, false
	switch {
	case s.isClosed():
		res = trackClosed
	case len(s.conns) >= s.maxConns:
		res, open = trackFull, len(s.conns)
		refusal = !s.refusing // the first refusal of an episode, and only it
		s.refusing = true
	default:
		s.refusing = false
		s.conns[conn] = struct{}{}
	}
	s.mu.Unlock()
	if refusal {
		s.logf("voya: dns: tcp: %d connections open, refusing %s and any more until one closes", open, conn.RemoteAddr())
	}
	return res
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

func (s *Server) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// Close stops the server, which makes [Server.Serve] return nil: both
// sockets are closed, and every TCP connection still open with them. It does
// not wait for Serve — the caller that started Serve is the one holding the
// result to wait on, and Serve returns only once the connection goroutines
// have finished, so waiting on Serve is waiting on everything.
//
// Safe to call more than once.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		s.mu.Lock()
		close(s.closed)
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
		err = s.ln.Close()
	})
	return err
}

// answer builds a reply to a query, reporting false when there is nothing to
// reply to at all — a reply needs the query's ID, and a reply must never be
// sent in answer to another reply.
//
// Everything else gets an answer, including the messages this server will not
// serve. Silence would leave the client waiting out its own timeout for
// something that is never coming, and a resolver timeout says nothing about
// why (ADR 005: the failure has to explain itself, not hang).
//
// limit is the largest reply the transport can carry: [maxUDPMessage] on UDP,
// [maxTCPMessage] on TCP. Both transports come through here, so a rule holds
// on both or on neither.
func answer(query []byte, limit int) ([]byte, bool) {
	if len(query) < headerLen {
		return nil, false
	}
	if query[2]&0x80 != 0 {
		return nil, false // a response, not a query
	}

	msg, ok := parse(query)
	if !ok {
		// The header parses, the question does not. FORMERR, with no
		// question section echoed back — there was none to echo.
		reply := replyHeader(query)
		reply[3] = 0x81 // RA + RCODE 1 (Format error)
		return reply, true
	}

	reply := replyHeader(query)
	reply = append(reply, query[headerLen:msg.questionEnd]...)
	reply[5] = 1 // QDCOUNT: the one question, echoed back

	answers := 0
	switch {
	case msg.qclass != classIN:
		// IN is the only class voya has anything in. A client asking CH, or
		// one setting mDNS' unicast-response bit in the class field, gets
		// told so rather than left waiting.
		reply[3] = 0x84 // RA + RCODE 4 (Not implemented)

	case !registry.IsInternal(msg.name):
		// Not ours to answer. REFUSED rather than NXDOMAIN: NXDOMAIN is a
		// claim that the name does not exist anywhere, and this server has
		// no standing to make it about the rest of the internet.
		reply[3] = 0x85 // RA + RCODE 5 (Refused)

	case msg.qtype == typeA:
		// A name long enough to push the answer past 512 bytes cannot be
		// served over plain UDP. TC tells the client to ask again over TCP,
		// where the same server answers it whole (#184) — and if the client
		// will not, an honest truncation flag still beats an oversized
		// datagram that resolvers discard without saying why.
		if len(reply)+len(msg.nameBytes)+aRecordLen > limit {
			reply[2] |= 0x02 // TC
			break
		}
		reply = appendA(reply, msg.nameBytes)
		answers = 1

	case msg.qtype == typeAAAA:
		// NOERROR with no answer, which is what tells a resolver "this name
		// exists but has no AAAA" so it asks for A. NXDOMAIN here would
		// make the whole name fail on any client that tries AAAA first.

	default:
		// Any other type for a name we own: exists, nothing to say.
	}

	// ANCOUNT is ours; NS and AR were zeroed with the rest of the header.
	reply[6], reply[7] = byte(answers>>8), byte(answers)
	return reply, true
}

// replyHeader starts a response: the query's ID, its flags rewritten as an
// answer, and every section count zeroed for the caller to fill in.
func replyHeader(query []byte) []byte {
	reply := make([]byte, 0, maxUDPMessage)
	reply = append(reply, query[:headerLen]...)

	// Same ID, QR=1 (response), same opcode and RD, RA=1.
	reply[2] = 0x80 | (query[2] & 0x78) | (query[2] & 0x01) // QR + opcode + RD
	reply[3] = 0x80                                         // RA, RCODE 0
	for i := 4; i < headerLen; i++ {
		reply[i] = 0
	}
	return reply
}

const (
	typeA    = 1
	typeAAAA = 28
	classIN  = 1

	// headerLen is the fixed DNS header: ID, flags, and four section counts.
	headerLen = 12

	// aRecordLen is what appendA writes after the name: type, class, TTL,
	// RDLENGTH and the four bytes of address.
	aRecordLen = 14
)

type question struct {
	name        string
	nameBytes   []byte // the wire-format QNAME, for echoing into the answer
	qtype       uint16
	qclass      uint16
	questionEnd int
}

// parse reads the first question of a message whose header [answer] has
// already checked. Anything after that question is ignored: this server
// answers one and has no use for EDNS options or additional records.
func parse(msg []byte) (question, bool) {
	qdcount := int(msg[4])<<8 | int(msg[5])
	if qdcount != 1 {
		// Zero questions leaves nothing to answer; more than one is legal
		// on paper and implemented by nobody.
		return question{}, false
	}

	var (
		labels []string
		offset = headerLen
		start  = offset
	)
	for {
		if offset >= len(msg) {
			return question{}, false
		}
		length := int(msg[offset])
		if length == 0 {
			offset++
			break
		}
		// No compression pointers in a question section.
		if length&0xC0 != 0 {
			return question{}, false
		}
		offset++
		if offset+length > len(msg) {
			return question{}, false
		}
		labels = append(labels, string(msg[offset:offset+length]))
		offset += length
	}
	if offset-start > maxNameLen {
		// Longer than a domain name may be, so the message is malformed and
		// the caller says FORMERR — the alternative being a reply larger
		// than either transport's limit (see [maxNameLen]).
		return question{}, false
	}
	if offset+4 > len(msg) {
		return question{}, false
	}
	qtype := uint16(msg[offset])<<8 | uint16(msg[offset+1])
	qclass := uint16(msg[offset+2])<<8 | uint16(msg[offset+3])
	offset += 4

	return question{
		name:        strings.Join(labels, "."),
		nameBytes:   msg[start : offset-4],
		qtype:       qtype,
		qclass:      qclass,
		questionEnd: offset,
	}, true
}

// appendA writes one A record pointing at loopback.
func appendA(reply, name []byte) []byte {
	reply = append(reply, name...)
	reply = append(reply, byte(typeA>>8), byte(typeA))
	reply = append(reply, byte(classIN>>8), byte(classIN))
	reply = append(reply, 0, 0, byte(TTL>>8), byte(TTL))
	reply = append(reply, 0, 4)
	return append(reply, Loopback.To4()...)
}
