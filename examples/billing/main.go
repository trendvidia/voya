// Command billing is a fleet service that calls another one by name.
//
// It does both halves of what voya is for: it registers itself, so it is
// reachable at http://billing.internal:9777, and it calls payments at
// http://payments.internal:9777 — the same dial string it would use on Fly,
// with no build tag and no environment branch in this file.
//
// Run it after payments:
//
//	voya run                 # terminal 1
//	go run ./payments        # terminal 2
//	go run ./billing         # terminal 3
//
// Then watch what voya makes possible. Restart payments and billing keeps
// calling the same name while the port underneath changes; stop payments and
// billing gets voya's diagnostic — the name that failed and what would have
// worked — rather than a bare connection error.
//
// The dialer is what makes `payments.internal` resolve without DNS: Join
// installs it on http.DefaultTransport, so http.DefaultClient and anything
// built on it route *.internal through voya's proxy. Nothing here is
// voya-specific except the import and that one call.
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
	"github.com/trendvidia/voya/examples/internal/demo"
)

func main() {
	every := flag.Duration("every", 3*time.Second, "how often to call payments")
	flag.Parse()

	if err := run(*every); err != nil {
		log.Fatalf("billing: %v", err)
	}
}

func run(every time.Duration) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ListenConfig rather than the shorter net.Listen because voya's own lint
	// rules require the context-aware form; a service outside this repository
	// can write either.
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: newMux(), Protocols: protocols, ReadHeaderTimeout: 10 * time.Second}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	stop, err := voya.Join(context.Background(), "billing", ln.Addr(),
		voya.WithLogf(func(format string, args ...any) { log.Printf("billing: "+format, args...) }),
	)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}

	log.Printf("billing: serving on %s, reachable at http://billing.internal:9777", ln.Addr())

	// By name, on voya's proxy port — the dial string this service would use
	// unchanged on Fly. http.DefaultClient carries the dialer Join installed,
	// so nothing here has to know that voya exists.
	payments := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		http.DefaultClient, "http://payments.internal:9777"+demo.Procedure)

	go callLoop(ctx, payments, every)

	<-ctx.Done()

	log.Print("billing: leaving the registry")
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

// newMux is everything this service answers. billing serves no procedures of
// its own, so that is only grpc.health.v1, which every fleet service serves
// (ADR 004) and `voya health` asks.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker()))
	return mux
}

// callLoop calls payments until ctx is done, reporting each outcome.
//
// A failure is printed and the loop carries on, because the interesting thing
// to watch here is what voya says when the callee is missing: stop payments
// and the error carries the authority that failed and what would have worked,
// which is ADR 005's diagnostic rather than a bare connection refused.
func callLoop(ctx context.Context, payments *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue], every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()

	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		reply, err := payments.CallUnary(callCtx,
			connect.NewRequest(wrapperspb.String(fmt.Sprintf("invoice-%03d", n))))
		cancel()

		if err != nil {
			log.Printf("billing: calling payments.internal: %v", err)
			continue
		}
		log.Printf("billing: payments said %q", reply.Msg.GetValue())
	}
}
