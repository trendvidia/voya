package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// upstreamProbeBudget bounds the look at a port voya has not spoken to yet.
// A server of either kind answers the preface at once over loopback, so
// running out means something accepted and went quiet, and that keeps the
// h2c path it always had.
const upstreamProbeBudget = time.Second

// http2Preface is what an HTTP/2 client opens with, followed by an empty
// SETTINGS frame. An h2c server answers it with its own SETTINGS. An HTTP/1
// server reads the leading "PRI * HTTP/2.0" as a request and refuses it.
const http2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00"

// http2FrameSettings is the type byte of a SETTINGS frame, the fourth byte of
// its header and the first frame an HTTP/2 server sends.
const http2FrameSettings = 4

// errGRPCNeedsHTTP2 refuses a gRPC call to an HTTP/1 service. gRPC carries its
// status in trailers, so sending the call anyway fails later and less clearly.
var errGRPCNeedsHTTP2 = errors.New("gRPC needs HTTP/2")

type upstreamProtocol int

const (
	upstreamUnknown upstreamProtocol = iota
	upstreamH2C
	upstreamHTTP1
	upstreamTLSHTTP2
	upstreamTLSHTTP1
)

// upstreamTLSConfig is how voya speaks TLS to a local service. It accepts any
// certificate, because a local service's can't match the 127.0.0.1 voya dials.
// That is safe only because the address is always loopback: registration
// carries a port and no host (ADR 003). If a target could ever name another
// host, this must not reach it.
func upstreamTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // Loopback only, as above.
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2", "http/1.1"},
	}
}

// probeUpstream asks a port which protocol it speaks, on a connection of its
// own, so the look happens before any of the request is sent. Trying h2c and
// retrying on failure can't do that: by then a streamed body is spent and a
// service may have acted on the request.
//
// The cost is visible to one kind of server. A Go HTTP/1.1 server hands
// "PRI * HTTP/2.0" to its handler, so a handler that isn't an http.ServeMux
// sees one PRI request on first contact. A ServeMux answers 400 itself. Pinned
// by TestTheProbeReachesOnlyABareHandler.
//
// upstreamUnknown means nothing was learned: the dial failed or nothing
// answered. The caller then takes the h2c path, whose errors are the ones
// ErrorHandler explains.
func probeUpstream(ctx context.Context, addr string) upstreamProtocol {
	ctx, cancel := context.WithTimeout(ctx, upstreamProbeBudget)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return upstreamUnknown
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var head [9]byte
	n := 0
	if _, err = io.WriteString(conn, http2Preface); err == nil {
		n, err = io.ReadFull(conn, head[:])
	}
	switch {
	case n == len(head) && head[3] == http2FrameSettings && head[0] != 'H':
		return upstreamH2C
	case n == 0 && isTimeout(err):
		return upstreamUnknown
	default:
		// Any other reply is not cleartext HTTP/2, and neither is hanging up,
		// which reads as an EOF or a reset. A TLS server can look like
		// HTTP/1.1 here: Go's answers in plain text.
		return probeUpstreamTLS(ctx, addr)
	}
}

func isTimeout(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

// probeUpstreamTLS tries a TLS handshake on a port that isn't cleartext
// HTTP/2. Success means TLS, and the handshake says which HTTP the service
// offers. Failure leaves HTTP/1.1. A plain HTTP/1.1 server sees one malformed
// request and no handler runs, pinned by TestTheProbeReachesOnlyABareHandler.
func probeUpstreamTLS(ctx context.Context, addr string) upstreamProtocol {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return upstreamHTTP1
	}
	defer func() { _ = conn.Close() }()
	client := tls.Client(conn, upstreamTLSConfig())
	if err := client.HandshakeContext(ctx); err != nil {
		return upstreamHTTP1
	}
	if client.ConnectionState().NegotiatedProtocol == "h2" {
		return upstreamTLSHTTP2
	}
	return upstreamTLSHTTP1
}

// upstreamRoundTripper sends each request in the protocol its upstream speaks:
// h2c, HTTP/1.1 or either over TLS, with no declaration from the service.
//
// An answer is remembered per address and forgotten when a request fails, so
// a port that a restarted service reuses with the other protocol costs one
// failed request. Concurrent first requests may each probe, which is harmless.
type upstreamRoundTripper struct {
	h2c, http1, tls http.RoundTripper
	probe           func(context.Context, string) upstreamProtocol

	mu    sync.Mutex
	known map[string]upstreamProtocol
}

func newUpstreamRoundTripper(idle time.Duration) *upstreamRoundTripper {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	tlsProtocols := new(http.Protocols)
	tlsProtocols.SetHTTP1(true)
	tlsProtocols.SetHTTP2(true)
	return &upstreamRoundTripper{
		h2c: upstreamTransport(idle),
		http1: &http.Transport{
			DialContext:     (&net.Dialer{}).DialContext,
			Protocols:       protocols,
			IdleConnTimeout: idle,
		},
		tls: &http.Transport{
			DialContext:     (&net.Dialer{}).DialContext,
			Protocols:       tlsProtocols,
			TLSClientConfig: upstreamTLSConfig(),
			IdleConnTimeout: idle,
		},
		probe: probeUpstream,
		known: map[string]upstreamProtocol{},
	}
}

func (u *upstreamRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	addr := r.URL.Host
	protocol := u.protocolOf(r.Context(), addr)
	if (protocol == upstreamHTTP1 || protocol == upstreamTLSHTTP1) && needsHTTP2(r) {
		return nil, errGRPCNeedsHTTP2
	}
	transport := u.h2c
	switch protocol {
	case upstreamHTTP1:
		transport = u.http1
	case upstreamTLSHTTP1, upstreamTLSHTTP2:
		transport = u.tls
		// The URL decides the dial, and Host still carries the service's
		// name, so the caller keeps its http:// address.
		secure := *r.URL
		secure.Scheme = "https"
		r = r.WithContext(r.Context())
		r.URL = &secure
	}
	res, err := transport.RoundTrip(r)
	if err != nil {
		u.forget(addr)
	}
	return res, err
}

func (u *upstreamRoundTripper) protocolOf(ctx context.Context, addr string) upstreamProtocol {
	u.mu.Lock()
	protocol := u.known[addr]
	u.mu.Unlock()
	if protocol != upstreamUnknown {
		return protocol
	}
	protocol = u.probe(ctx, addr)
	if protocol != upstreamUnknown {
		u.mu.Lock()
		u.known[addr] = protocol
		u.mu.Unlock()
	}
	return protocol
}

func (u *upstreamRoundTripper) forget(addr string) {
	u.mu.Lock()
	delete(u.known, addr)
	u.mu.Unlock()
}

// needsHTTP2 reports a gRPC call. gRPC-Web is excluded: it carries its
// trailers in the body, so it survives HTTP/1.1.
func needsHTTP2(r *http.Request) bool {
	return isGRPC(r) && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc-web")
}
