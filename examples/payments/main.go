// Command payments is a fleet service, written the way a real one is.
//
// It binds an ephemeral port, hands the bound address to [voya.Join], and
// serves. From then on it is reachable at http://payments.internal:9777 —
// from billing, from `voya call`, and from curl once `voya dns install` has
// run — while nothing anywhere is configured with the port it actually got.
//
// Run it:
//
//	voya run                 # terminal 1
//	go run ./payments        # terminal 2
//	voya status              # terminal 3
//
// Start a second copy and voya evicts the first, loudly, in both terminals:
// that is ADR 002's evict-and-replace, and it is what makes restarting a
// service during development a non-event rather than a port conflict.
//
// -instance labels this copy in its own log lines and in what it returns, so
// two copies of one name can be told apart while you watch an eviction.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"google.golang.org/protobuf/types/known/wrapperspb"

	voya "github.com/trendvidia/voya/client"
	voyareflect "github.com/trendvidia/voya/client/reflection"
	"github.com/trendvidia/voya/examples/internal/demo"
)

func main() {
	instance := flag.String("instance", "first", "label for this copy, so two of them can be told apart")
	flag.Parse()

	if err := run(*instance); err != nil {
		log.Fatalf("payments: %v", err)
	}
}

func run(instance string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Port 0: the OS chooses. Nothing else in the fleet is told which one it
	// picked — that is the whole point, and it is what makes two copies of
	// this service able to run at once.
	//
	// ListenConfig rather than the shorter net.Listen because voya's own lint
	// rules require the context-aware form; a service outside this repository
	// can write either.
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	mux, err := newMux(instance)
	if err != nil {
		return err
	}

	// Cleartext HTTP/2 as well as HTTP/1.1. Both halves matter: voya speaks
	// HTTP/1.1 to a service that offers nothing else, but gRPC needs HTTP/2
	// end to end, and reflection is a bidirectional stream that a default
	// http.Server answers with 505 HTTP Version Not Supported — which every
	// client reads as "this service has no schema".
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: 10 * time.Second}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	// The one call. It never blocks and never fails because voya is not
	// running: the registration stream is opened in the background and
	// retried with bounded backoff, so this service starts and serves
	// normally with no voya at all — degrading only in that nothing can reach
	// it by name. On Fly, where FLY_APP_NAME is set, it does nothing at all.
	stop, err := voya.Join(context.Background(), "payments", ln.Addr(),
		voya.WithLogf(func(format string, args ...any) {
			log.Printf("payments[%s]: "+format, append([]any{instance}, args...)...)
		}),
	)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}

	log.Printf("payments[%s]: serving on %s, reachable at http://payments.internal:9777", instance, ln.Addr())

	<-ctx.Done()

	// Deregister first, drain second, and the order is the point (ADR 004).
	// stop returns once voya has had the shutdown grace to stop routing here,
	// so by the time the listener closes there is nothing left being sent to
	// it. The other way round cuts off whatever voya routed in between, which
	// a developer reads as flakiness in their own service.
	log.Printf("payments[%s]: leaving the registry", instance)
	stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// newMux is everything this service answers.
func newMux(instance string) (*http.ServeMux, error) {
	mux := http.NewServeMux()

	// grpc.health.v1, which every fleet service serves (ADR 004): it is what
	// `voya health` asks. A static checker always says SERVING; a service with
	// something that can fail, such as a database, implements
	// grpchealth.Checker instead.
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(demo.Service)))

	// Reflection, so `voya call` can ask this service for its own schema
	// instead of being told it. A real service passes the service-name
	// constant its generated Connect package declares and calls Mount; this
	// one has no generated code, so it passes a hand-built schema to
	// MountFrom. See the demo package for why.
	if err := voyareflect.MountFrom(mux, demo.Schema(), demo.Service); err != nil {
		return nil, fmt.Errorf("mounting reflection: %w", err)
	}

	mux.Handle(demo.Procedure, connect.NewUnaryHandler(demo.Procedure,
		func(_ context.Context, req *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			// req.Header().Get("Host") is empty over HTTP/2; connect-go
			// exposes the authority through the request's own spec instead.
			// What matters for the demo is that the caller dialled a *name*:
			// this service never learns the port it was reached on.
			charged := req.Msg.GetValue()
			if charged == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					errors.New("charge: empty amount"))
			}
			log.Printf("payments[%s]: charged %s", instance, charged)
			return connect.NewResponse(wrapperspb.String(
				fmt.Sprintf("payments[%s] charged %s", instance, charged))), nil
		}))

	return mux, nil
}
