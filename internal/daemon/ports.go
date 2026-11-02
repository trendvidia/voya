package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/lsof"
)

// Ports is one free port for each listener voya binds.
//
// It exists for tests — the acceptance suite and this package's own — which
// need a daemon on ports nothing else holds. It lives here, in the package
// that owns the listener table, rather than in a helper beside each caller,
// because the table is the thing that has to be got right: see [FreePorts].
type Ports struct {
	Proxy        uint32
	Registration uint32
	DNS          uint32
}

// FreePorts draws a free port for each of voya's three listeners, each on the
// transport that listener actually binds, and returns three distinct numbers.
//
// Drawing from [listener] is the whole point, and it is not tidiness. Both
// callers used to keep their own copy of this, and until #44's review both
// drew every port with net.Listen on TCP — including the DNS port, which
// dns.Listen binds with ListenPacket on UDP (and, since #184, on TCP at the
// same number as well). TCP and UDP are separate port spaces, so a TCP
// reservation says nothing about whether the UDP port is free: a browser
// holding one for QUIC (which they do, for minutes) was invisible to every
// draw, and voya then failed to start on a port the test had just been told
// was available. Retrying could not help, because the retry re-rolled the
// same blind dice. Correcting it meant correcting it twice, in two files,
// with nothing but review standing between the second copy and being missed.
//
// Deriving the transport from the same table the daemon uses is what stops
// that being a thing a caller can get wrong. Adding a fourth listener means
// adding it there, and the ports follow.
//
// The three have to be distinct because [config.Validate] compares the
// numbers, and since #184 they are so by construction: [drawPorts] holds
// every socket at once, every one of the three numbers is held on TCP by
// exactly one of them — the DNS number included, because [reserve] binds it
// on TCP too and redraws when that fails — and the kernel does not hand a
// bound port to a second socket. So the retry below can no longer fire. It
// stays as the check that would say so if a fourth listener drew on a
// transport no other one holds, which is how a UDP-only DNS draw used to
// collide with a TCP one.
func FreePorts(ctx context.Context) (Ports, error) {
	for range 10 {
		ports, err := drawPorts(ctx)
		if err != nil {
			return Ports{}, err
		}
		if ports.Proxy != ports.Registration && ports.Proxy != ports.DNS && ports.Registration != ports.DNS {
			return ports, nil
		}
	}
	return Ports{}, errors.New("daemon: no three distinct free ports in ten draws")
}

// drawPorts holds a reservation open for each listener at once, and reads all
// three numbers before releasing any of them.
//
// All three at once rather than one after another: the kernel will not hand a
// bound port to a second socket, so the two TCP draws come back distinct by
// construction instead of by [FreePorts] noticing and drawing again, and no
// port sits released for the span of the draws that follow it.
func drawPorts(ctx context.Context) (Ports, error) {
	proxy, proxySock, err := reserve(ctx, proxyListener)
	defer release(proxySock)
	if err != nil {
		return Ports{}, err
	}
	registration, registrationSock, err := reserve(ctx, registrationListener)
	defer release(registrationSock)
	if err != nil {
		return Ports{}, err
	}
	dns, dnsSock, err := reserve(ctx, dnsListener)
	defer release(dnsSock)
	if err != nil {
		return Ports{}, err
	}
	return Ports{Proxy: proxy, Registration: registration, DNS: dns}, nil
}

// release gives a reservation back, tolerating the nil that a draw which
// never bound anything reports.
func release(sock io.Closer) {
	if sock != nil {
		_ = sock.Close()
	}
}

// reserve binds an ephemeral port on loopback for each of l's transports and
// reports the number with the sockets still open, for the caller to give back.
//
// It hands those sockets back alongside an error too, because a bind that
// succeeded still holds a port when a later step fails: the caller releases
// whatever it is given, error or not.
//
// A listener on two transports — DNS, on UDP and TCP since #184 — needs one
// number free on both. The first transport draws the number and the rest are
// bound to it; if one of them is taken, the whole draw is released and made
// again, [reserveAttempts] times, because TCP and UDP are separate port
// spaces and the kernel's ephemeral choice on one says nothing about the
// other.
//
// The window between the release and the caller's bind is real and
// unavoidable — the OS will not hold a port for a process that is not
// listening on it — and it is short, since it opens only once the whole draw
// is done. The acceptance suite retries around it; this package's own tests
// do not, so a port stolen inside it fails a test rather than misleading one.
// What it is not is the defect described on [FreePorts]: that one made the
// reservation meaningless rather than merely brief.
func reserve(ctx context.Context, l listener) (uint32, io.Closer, error) {
	if len(l.networks) == 0 {
		return 0, nil, fmt.Errorf("daemon: the %s listener declares no network", l.name)
	}
	var lastErr error
	for range reserveAttempts {
		var (
			held   sockets
			number uint32
			err    error
		)
		for i, network := range l.networks {
			var sock io.Closer
			var bound int
			sock, bound, err = bind(ctx, l, network, number)
			if sock != nil {
				held = append(held, sock)
			}
			if err != nil {
				if i == 0 || errors.Is(err, errUnknownNetwork) {
					// The first draw failing, or a network nobody
					// handles, is not the collision retrying is for.
					return 0, held, err
				}
				break
			}
			if i == 0 {
				if number, err = portNumber(l, bound); err != nil {
					return 0, held, err
				}
			}
		}
		if err == nil {
			return number, held, nil
		}
		lastErr = err
		release(held)
	}
	return 0, nil, fmt.Errorf("daemon: no port free on every transport of the %s listener in %d draws: %w",
		l.name, reserveAttempts, lastErr)
}

// reserveAttempts bounds how many times [reserve] redraws a number that
// turned out to be taken on one of a listener's other transports.
const reserveAttempts = 10

// errUnknownNetwork is what [bind] reports for a network the switch in it
// does not handle, so [reserve] can tell it from a collision.
var errUnknownNetwork = errors.New("neither TCP nor UDP")

// sockets is every socket one draw holds, released together.
type sockets []io.Closer

func (s sockets) Close() error {
	errs := make([]error, 0, len(s))
	for _, sock := range s {
		errs = append(errs, sock.Close())
	}
	return errors.Join(errs...)
}

// bind holds port (0 for the kernel's choice) on loopback for one of l's
// transports and reports the number it got. On failure the socket returned is
// nil unless one was bound and then found wanting, in which case the caller
// still has to release it.
func bind(ctx context.Context, l listener, network lsof.Network, port uint32) (io.Closer, int, error) {
	var lc net.ListenConfig
	switch network {
	case lsof.TCP:
		ln, err := lc.Listen(ctx, "tcp", config.Addr(port))
		if err != nil {
			return nil, 0, fmt.Errorf("daemon: reserving a tcp port for the %s listener: %w", l.name, err)
		}
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			return ln, 0, fmt.Errorf("daemon: %s listener address is %T, want *net.TCPAddr", l.name, ln.Addr())
		}
		return ln, addr.Port, nil
	case lsof.UDP:
		conn, err := lc.ListenPacket(ctx, "udp", config.Addr(port))
		if err != nil {
			return nil, 0, fmt.Errorf("daemon: reserving a udp port for the %s listener: %w", l.name, err)
		}
		addr, ok := conn.LocalAddr().(*net.UDPAddr)
		if !ok {
			return conn, 0, fmt.Errorf("daemon: %s listener address is %T, want *net.UDPAddr", l.name, conn.LocalAddr())
		}
		return conn, addr.Port, nil
	}
	return nil, 0, fmt.Errorf("daemon: the %s listener declares network %q, which is %w", l.name, network, errUnknownNetwork)
}

// portNumber narrows the int the net package reports to the uint32 the config
// message carries.
//
// A kernel that just bound an ephemeral port cannot answer outside 1..65535,
// so this is a conversion rather than a check — but it is a conversion the
// compiler cannot see is safe, and answering 0 on a surprise would send the
// daemon to an ephemeral port of its own choosing while the test believed it
// had picked one.
func portNumber(l listener, n int) (uint32, error) {
	if n <= 0 || n > 65535 {
		return 0, fmt.Errorf("daemon: the %s listener bound port %d, which is not a port", l.name, n)
	}
	return uint32(n), nil
}
