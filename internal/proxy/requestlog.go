package proxy

import (
	"net"
	"net/http"
	"strings"
)

// recordedAuthority is the authority one record carries: the :authority or
// Host header the request arrived with, less voya's own proxy port.
//
// The port is dropped because the column exists to be grepped for a name and
// voya's port says nothing there (#161). An h2 client sends
// `payments.internal:9777`, an HTTP/1.1 client setting Host sends
// `payments.internal`, and the same service reading two ways depending on
// which caller made the request is what the strip removes: both record as
// `payments.internal`.
//
// A *foreign* port is kept exactly as it arrived. A caller that reached voya
// while naming some other port in its authority is the one thing this column
// should show about a port, and the header is the only place that mistake is
// visible — routing itself ignores it (registry.ServiceFromAuthority).
//
// "voya's own" is the port the request arrived on, read off the connection the
// way [proxy.isSelf] reads it: the proxy is handed a listener rather than told
// an address, so the request's context is the only thing here that knows which
// port answered. A request that came off no listener — a handler test — is
// recorded as it arrived.
func recordedAuthority(r *http.Request) string {
	colon := strings.LastIndexByte(r.Host, ':')
	if colon < 0 {
		return r.Host // no port to weigh; the common HTTP/1.1 shape
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return r.Host
	}
	_, own, err := net.SplitHostPort(local.String())
	if err != nil || r.Host[colon+1:] != own {
		return r.Host
	}
	return r.Host[:colon]
}

// statusWriter is the ResponseWriter the proxy hands the rest of itself when
// there is a request log to fill: it remembers the status the handler wrote,
// which net/http gives a handler no other way to learn, and passes everything
// else straight through.
//
// Straight through matters more than it looks. httputil.ReverseProxy flushes
// a streaming response through http.NewResponseController, which reaches the
// real writer by calling Unwrap on whatever it was handed; connect-go's
// handlers do the same for a server stream. A wrapper without Unwrap would
// turn every streamed response into one delivered at the end, and this
// package's own TestResponseIsNotBuffered would be the first to say so.
// Flush is implemented as well as unwrapped, for callers that still type-
// assert http.Flusher rather than go through the controller.
type statusWriter struct {
	http.ResponseWriter
	code int
}

// WriteHeader records the first *final* status written. net/http ignores a
// second call with a warning, so the first one from 200 up is the one on the
// wire.
//
// A 1xx is not that call. net/http lets a handler send any number of
// informational responses ahead of the real one, and
// httputil.ReverseProxy forwards an upstream's through this very writer —
// its Got1xxResponse hook calls WriteHeader(code) for each. Latching one
// would record 103 for a request the caller saw answered 200, and 100 for
// every HTTP/1.1 caller that sent Expect: 100-continue.
// TestAnEarlyHintIsNotTheStatus holds it to that.
func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 && code >= 200 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write is where an implicit 200 happens: a handler that writes a body
// without WriteHeader gets one from net/http, and so does the record.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer's Flusher, if it has one, and is
// otherwise a no-op — the same contract http.ResponseController.Flush keeps.
func (w *statusWriter) Flush() {
	// Through the controller rather than a type assertion, so a writer that
	// itself only unwraps to a Flusher still flushes.
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap is what http.ResponseController walks to reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// status is what went on the wire: the status written, or the 200 net/http
// sends for a handler that returned without writing anything at all.
func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
