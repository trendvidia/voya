// Package registration serves ADR 004's registration stream: the long-lived
// bidirectional Connect stream on 127.0.0.1:9779 that a service opens at
// startup and holds for its lifetime.
//
// The stream *is* the liveness signal. There is no polling, no health probe
// and no TTL — nothing here runs on a timer per service. What a registration
// does cost is two goroutines that both end with the stream: the handler, and
// one pumping the stream's receive side so the handler can select over reads,
// eviction and cancellation together. ADR 004's "no per-service goroutine
// beyond the stream handler" is a claim about the background probe workers it
// rejected, and this is the whole of what replaced them.
//
// When the stream ends for any reason the service is evicted, which makes
// eviction as fast as TCP close detection and makes a voya restart a
// non-event — every stream breaks at once and the services re-register on
// their own.
//
// Framing is plain Connect streaming. protowire-go/envelope was evaluated for
// this and rejected (#8): Connect already frames and types every message, and
// carrying the payload as opaque bytes inside an envelope would erase the
// schema that `buf breaking` checks.
package registration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/trendvidia/voya/internal/registry"
	registryv1 "github.com/trendvidia/voya/proto/voya/registry/v1"
	"github.com/trendvidia/voya/proto/voya/registry/v1/registryv1connect"
)

// DefaultRegistrationTimeout bounds how long a stream may stay open without
// registering anything.
//
// A client sends its one message immediately on connect, so this is generous
// by two orders of magnitude — it exists only so that a connection which
// opens and then says nothing cannot hold a goroutine and a registry-plane
// connection forever.
//
// It applies only before the first message. Once a service is registered its
// stream is silent by design, and a timer that outlived registration would
// evict every healthy service on an interval — the polling design ADR 004
// rejected, arrived at by accident. [TestRegisteredStreamOutlivesTheTimeout]
// pins that.
const DefaultRegistrationTimeout = 30 * time.Second

// Server implements voya.registry.v1.RegistrationService.
type Server struct {
	reg       *registry.Registry
	proxyPort uint32
	timeout   time.Duration
}

// Option configures a Server.
type Option func(*Server)

// WithRegistrationTimeout replaces [DefaultRegistrationTimeout]. Zero or
// negative disables the limit, matching the zero ReadTimeout in
// [NewHTTPServer] and http.Server's own convention — and, more to the point,
// making an unset duration mean "no bound" rather than "reject every
// registration instantly", which is what a timeout of 0 would otherwise do.
func WithRegistrationTimeout(d time.Duration) Option {
	return func(s *Server) { s.timeout = d }
}

// NewServer returns a registration service backed by reg. proxyPort is echoed
// in the acknowledgement so a developer reads a complete dial string at
// startup rather than a name they then have to pair with a port.
func NewServer(reg *registry.Registry, proxyPort uint32, opts ...Option) *Server {
	s := &Server{reg: reg, proxyPort: proxyPort, timeout: DefaultRegistrationTimeout}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler returns the mount path and handler for the registration service.
func (s *Server) Handler(opts ...connect.HandlerOption) (string, http.Handler) {
	return registryv1connect.NewRegistrationServiceHandler(s, opts...)
}

// received is one result from the stream's receive side.
type received struct {
	msg *registryv1.RegisterRequest
	err error
}

// Register implements the registration stream.
//
// The shape is: read one message, register, acknowledge, then hold until
// something ends it. Everything interesting is in what counts as "ends it" —
// see the loop below.
func (s *Server) Register(
	ctx context.Context,
	stream *connect.BidiStream[registryv1.RegisterRequest, registryv1.RegisterResponse],
) error {
	msgs := s.receive(ctx, stream)

	// Built before the select and never rearmed: the bound is on getting a
	// first message, not on the registration that follows it.
	var timeout <-chan time.Time
	if s.timeout > 0 {
		t := time.NewTimer(s.timeout)
		defer t.Stop()
		timeout = t.C
	}

	var first *registryv1.RegisterRequest
	select {
	case r, ok := <-msgs:
		switch {
		case !ok:
			// The receive side gave up because the request context ended.
			return nil
		case errors.Is(r.err, io.EOF):
			// Opened and closed again without registering. Nothing was ever
			// put in the registry, so there is nothing to undo.
			return nil
		case r.err != nil:
			// Not a disconnect: the message did not decode, or the
			// connection failed part-way through one. Connect's error already
			// carries a code and the reason, and a client looping on a
			// message voya cannot read needs to be told that (ADR 005) rather
			// than shown a clean end it is entitled to retry forever.
			return r.err
		}
		first = r.msg
	case <-timeout:
		return connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf(
			"no registration received within %s: a service must send its name and port as soon as it opens the stream",
			s.timeout))
	case <-ctx.Done():
		return nil
	}

	inst := registry.Instance{
		Service: first.GetService(),
		Port:    first.GetPort(),
		PID:     first.GetPid(),
	}
	register := s.reg.Register
	if first.GetOnlyIfUnclaimed() {
		// A deposed instance asking for its name back rather than taking it
		// (#27). Refusing is the answer that terminates: last-writer-wins
		// here would let the two trade the name forever.
		register = s.reg.RegisterIfFree
	}
	handle, err := register(inst)
	if errors.Is(err, registry.ErrNameHeld) {
		// AlreadyExists, not InvalidArgument: the request was well formed
		// and the answer may differ later, so a client is right to ask
		// again. InvalidArgument would tell it to stop for good.
		return connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		// The name or port is unusable. InvalidArgument rather than a
		// transport failure: no retry will fix it, and the registry's error
		// already says which rule was broken and why it exists.
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	// Closing on every exit from here is what makes the stream the liveness
	// signal: the name goes when the last of the instance's streams ends.
	// Generation-checked inside the registry, so an eviction that already
	// happened does not remove the replacement.
	defer handle.Close()

	if err := stream.Send(&registryv1.RegisterResponse{
		Event: &registryv1.RegisterResponse_Registered{
			Registered: &registryv1.Registered{
				Authority: registry.Authority(inst.Service),
				ProxyPort: s.proxyPort,
			},
		},
	}); err != nil {
		return err
	}

	for {
		select {
		case <-handle.Evicted():
			// The record the registry kept, not a copy reassembled here: it
			// is the same value `voya status` will show, so the two cannot
			// describe one eviction differently.
			ev, _ := handle.Eviction()
			// Best effort: the point of sending this is that the client can
			// tell an eviction from a dropped stream and decline to fight
			// for the name (#27). If the send fails the client is already
			// gone, which settles the question a different way.
			_ = stream.Send(&registryv1.RegisterResponse{
				Event: &registryv1.RegisterResponse_Evicted{
					Evicted: &registryv1.Evicted{
						Reason:         ev.String(),
						ReplacedByPort: ev.Incoming.Port,
						// Zero when the incoming client did not supply a PID,
						// which the field's documentation allows for.
						ReplacedByPid: ev.Incoming.PID,
					},
				},
			})
			// A clean end, not an error: the typed message above is the
			// signal, and a Connect error would arrive as a transport
			// failure that a client is entitled to retry.
			return nil

		case r, ok := <-msgs:
			switch {
			case !ok:
				// The receive side finished. Disable this case — a closed
				// channel is always ready, and leaving it selected would
				// spin. ctx below is the liveness signal from here on.
				msgs = nil
			case errors.Is(r.err, io.EOF):
				// The client half-closed its request side and may still be
				// perfectly alive, reading responses. Evicting here would
				// depose a healthy service, so hold and let ctx decide.
				msgs = nil
			case r.err != nil:
				// The stream broke: the process died, the connection was
				// reset, or a keepalive ping went unanswered. This is the
				// eviction path ADR 004 is built around.
				return nil
			default:
				// ADR 004's protocol is one message then silence. A second
				// one means the client and the server disagree about what
				// this stream is, and guessing which is right would be
				// worse than saying so.
				return connect.NewError(connect.CodeInvalidArgument, errors.New(
					"a registration stream carries exactly one message: close this stream, then open another, to change the registered port"))
			}

		case <-ctx.Done():
			return nil
		}
	}
}

// receive pumps the stream's receive side into a channel so the handler can
// select on it alongside eviction and cancellation.
//
// The goroutine ends when Receive fails, which it does as soon as the handler
// returns and Connect cancels the request context — so it cannot outlive the
// stream it belongs to.
func (s *Server) receive(
	ctx context.Context,
	stream *connect.BidiStream[registryv1.RegisterRequest, registryv1.RegisterResponse],
) <-chan received {
	out := make(chan received, 1)
	go func() {
		defer close(out)
		for {
			msg, err := stream.Receive()
			select {
			case out <- received{msg: msg, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}
