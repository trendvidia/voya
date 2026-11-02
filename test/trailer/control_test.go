package trailertest

import (
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// stripTrailers wraps a handler in one that deliberately discards trailers —
// deleting the http.TrailerPrefix entries the wrapped handler set, just before
// returning, which is when net/http would otherwise emit them as HTTP/2
// trailers.
func stripTrailers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		header := w.Header()
		for k := range header {
			if strings.HasPrefix(k, http.TrailerPrefix) {
				delete(header, k)
			}
		}
		delete(header, "Trailer")
	})
}

// invokeFail calls the procedure that answers NotFound through a voya
// listening on addr, and hands back what the client made of it.
func invokeFail(t *testing.T, addr string) error {
	t.Helper()
	cc := dial(t, addr, grpc.WithAuthority(proxyAuthority))
	return cc.Invoke(ctx(t), procFail, &emptypb.Empty{}, &emptypb.Empty{})
}

// TestNegativeControl_StrippedTrailersAreDetected proves the harness can
// actually fail.
//
// A passing test suite is worthless if it would pass no matter what. This runs
// the four cases' own path — voya's handler, through the same proxy.Listen and
// proxy.NewHTTPServer — with [stripTrailers] wrapped around it. Sabotaging the
// path under test rather than one standing beside it is what makes a pass here
// mean the four cases would notice if voya stopped forwarding trailers, and
// not merely that some proxy would.
//
// If the other tests are meaningful, this one must observe the *specific*
// degradation trailer loss causes.
func TestNegativeControl_StrippedTrailersAreDetected(t *testing.T) {
	backendAddr := serveH2C(t, backend())

	// The same handler, the same server and the same authority, minus the
	// sabotage. Establishing that this leg really does report NotFound is what
	// rules out the second way to reach a non-NotFound code, now that a
	// routing decision sits in the path where there was none before: an
	// authority the registry does not resolve answers Unimplemented, out of
	// voya's own 404 diagnostic, with every trailer forwarded intact. Without
	// this check a broken serviceName or a broken ServiceFromAuthority would
	// leave the control green while it had stopped testing trailer loss.
	if intact := status.Code(invokeFail(t, proxyInFront(t, backendAddr, nil))); intact != codes.NotFound {
		t.Fatalf("the unsabotaged path reports %v, not NotFound — nothing this test "+
			"observes with trailers stripped would be about trailers", intact)
	}

	err := invokeFail(t, proxyInFront(t, backendAddr, stripTrailers))
	code := status.Code(err)

	t.Logf("with trailers stripped: err=%v code=%v", err, code)

	if code == codes.NotFound {
		t.Fatal("stripping trailers did not change the observed code — " +
			"the harness is not sensitive to trailer loss, so the passing " +
			"tests prove nothing")
	}
	// The exact degradation ADR 003 and the README both quote, rather than
	// "any code but NotFound": a call that fails for some unrelated reason
	// also clears that weaker bar. Version-bound with everything else here —
	// this is grpc-go v1.83.0 reading a stream that ended with no status.
	if code != codes.Internal {
		t.Fatalf("want the documented degradation Internal, got %v (%q) — the call "+
			"failed, but not in the way trailer loss fails", code, status.Convert(err).Message())
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "trailers") {
		t.Errorf("want a message naming the missing trailers, got %q", msg)
	}
	t.Logf("harness is sensitive: NotFound degraded to %v when trailers were dropped", code)
}
