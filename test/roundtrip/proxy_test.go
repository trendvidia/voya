package roundtriptest

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
)

// registry is the spike's stand-in for ADR 004's registration stream: a
// hardcoded table, populated once at construction.
//
// It carries two sets on purpose. upstream is what is running right now;
// declared is every name voya has heard of. Without the second set, a service
// whose stream dropped is indistinguishable from a typo, and those two
// failures want completely different advice.
type registry struct {
	upstream map[string]string // hostname -> address of a running instance
	declared []string          // every known name, running or not
}

func (r *registry) lookup(host string) (addr string, running bool) {
	addr, running = r.upstream[host]
	return addr, running
}

func (r *registry) isDeclared(host string) bool { return slices.Contains(r.declared, host) }

func (r *registry) running() []string {
	names := make([]string, 0, len(r.upstream))
	for name := range r.upstream {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (r *registry) notRunning() []string {
	var names []string
	for _, name := range r.declared {
		if _, ok := r.upstream[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// normalizeHost folds a hostname to its canonical form: case dropped, FQDN
// trailing dot removed. DNS says those are the same name, and a developer who
// typed PAYMENTS.internal has not asked for a different service.
//
// Single-sourced because two consumers must agree on it exactly. The dialer
// (isInternal) decides on this rule whether a name diverts to voya, and the
// proxy (hostname) decides on the same rule what it routes to; if they ever
// disagree, a name is diverted and then fails to route — the worst of both
// answers. #16 and #17 split these into separate packages, which is precisely
// when two copies of one rule stop being edited together.
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// hostname reduces an authority to the routing key.
//
// The port is dropped: voya listens on one port and a name is a name whatever
// port the caller wrote after it.
func hostname(authority string) string {
	host := authority
	if h, _, err := net.SplitHostPort(authority); err == nil {
		host = h
	}
	return normalizeHost(host)
}

// upstreamKey carries the resolved upstream from the routing decision to the
// rewrite hook, which has no way to report an error of its own.
type upstreamKey struct{}

// diagnosticStyle selects how an unroutable request is answered.
type diagnosticStyle int

const (
	// plainText is ADR 005 taken literally: a human-readable body. Perfect
	// through curl, and — as TestDiagnosticIsInvisibleToConnectClients shows —
	// invisible to a connect-go caller.
	plainText diagnosticStyle = iota

	// connectEnvelope wraps the same text in the Connect error envelope when
	// the request is a Connect unary call, so the message survives into a Go
	// caller's error. Demonstrated, not adopted: see issue #23.
	connectEnvelope
)

// authorityHandling selects whether the proxy restores the authority that
// httputil's SetURL destroys.
//
// It exists so that the negative control is the real proxy minus exactly one
// line, rather than a second implementation that resembles it today. A
// control which has drifted from the code it controls still passes and still
// proves nothing — and #17 moving the restore elsewhere (into the Transport,
// a RoundTripper, a Director) is exactly the drift to guard against.
type authorityHandling int

const (
	restoreAuthority authorityHandling = iota
	naiveSetURL
)

// newProxy is the spike's version of #17: an h2c reverse proxy that routes on
// the authority and explains itself when it cannot.
func newProxy(reg *registry) http.Handler {
	return newProxyWith(reg, plainText, restoreAuthority)
}

func newProxyWithDiagnostics(reg *registry, style diagnosticStyle) http.Handler {
	return newProxyWith(reg, style, restoreAuthority)
}

// newNaiveProxy is newProxy without the Out.Host restore: the mistake a
// reasonable person writes from the httputil documentation's first example.
// It exists only so the negative control has something to fail against, and
// it comes from the same constructor so the difference stays the one line it
// claims to be.
func newNaiveProxy(reg *registry) http.Handler {
	return newProxyWith(reg, plainText, naiveSetURL)
}

func newProxyWith(reg *registry, style diagnosticStyle, authority authorityHandling) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			upstream, _ := pr.In.Context().Value(upstreamKey{}).(string)
			pr.SetURL(&url.URL{Scheme: "http", Host: upstream})

			// The single most important line in the spike. SetURL sets
			// Out.Host = "", which makes the outbound authority the upstream's
			// ephemeral address — the request still arrives, and the name the
			// whole design routes on is gone. Restoring it is what keeps
			// :authority end to end, and omitting it is the whole of
			// naiveSetURL.
			if authority == restoreAuthority {
				pr.Out.Host = pr.In.Host
			}
		},
		Transport: upstreamTransport(),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A registered service that will not accept a connection: crashed
			// between registration and now, or listening elsewhere.
			writeDiagnostic(w, r, http.StatusBadGateway, reg, style, fmt.Sprintf(
				"voya: %s is registered but the connection failed: %v",
				quote(r.Host), err))
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" {
			// Nothing to route on. ADR 005: say so, rather than 502.
			writeDiagnostic(w, r, http.StatusBadRequest, reg, style,
				"voya: the request arrived with no :authority (HTTP/2) or Host (HTTP/1) header.\n"+
					"voya routes by service name, so there is nothing to route on.")
			return
		}

		host := hostname(r.Host)
		upstream, running := reg.lookup(host)
		switch {
		case running:
			rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), upstreamKey{}, upstream)))

		case reg.isDeclared(host):
			// Known name, no instance. The developer's service is not the
			// problem; their other terminal is.
			writeDiagnostic(w, r, http.StatusServiceUnavailable, reg, style, fmt.Sprintf(
				"voya: service %s is known but has no running instance.\n"+
					"It registered earlier and its stream dropped, or it has not started yet.\n"+
					"Authority received: %s",
				quote(host), quote(r.Host)))

		default:
			// Never registered under any name voya has seen. Usually a typo.
			writeDiagnostic(w, r, http.StatusNotFound, reg, style, fmt.Sprintf(
				"voya: no service named %s.\n"+
					"Nothing has ever registered that name.\n"+
					"Authority received: %s",
				quote(host), quote(r.Host)))
		}
	})
}

// upstreamTransport dials backends over cleartext HTTP/2. gRPC needs HTTP/2
// on both legs, and the backend leg has no TLS to negotiate over, so this is
// prior-knowledge h2c: UnencryptedHTTP2 with HTTP1 off.
func upstreamTransport() *http.Transport {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Transport{
		DialContext: (&net.Dialer{}).DialContext,
		Protocols:   p,
	}
}

// writeDiagnostic is ADR 005's decision made concrete: the error response is
// the primary diagnostic surface. It names what arrived, says why it failed,
// and lists what would have worked — in the response the developer is already
// looking at, not in a log they have to think to check.
//
// Plain text is what a human reads through curl. pxf is for structured CLI
// output, and JSON only appears in the Connect error envelope, which is fixed
// by that protocol's spec rather than chosen here.
func writeDiagnostic(w http.ResponseWriter, r *http.Request, status int, reg *registry, style diagnosticStyle, reason string) {
	var b strings.Builder
	b.WriteString(reason)
	b.WriteString("\n\n")

	if running := reg.running(); len(running) > 0 {
		fmt.Fprintf(&b, "Registered services: %s\n", strings.Join(running, ", "))
	} else {
		b.WriteString("Registered services: none — nothing has registered with voya yet.\n")
	}
	if idle := reg.notRunning(); len(idle) > 0 {
		fmt.Fprintf(&b, "Known but not running: %s\n", strings.Join(idle, ", "))
	}

	if style == connectEnvelope && isConnectUnary(r) {
		writeConnectError(w, status, b.String())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprint(w, b.String())
}

// isConnectUnary reports whether the request is a Connect unary call, which
// is the shape whose error envelope is plain JSON. Streaming Connect
// (application/connect+*), gRPC and gRPC-Web put the outcome in an
// end-of-stream envelope or in trailers instead.
//
// The discriminator is the protocol's own version marker rather than the
// content type. connect-go sets Connect-Protocol-Version: 1 on every Connect
// request, and for a side-effect-free unary call sent as GET it moves the
// marker to a connect=v1 query parameter and drops the content type entirely
// (protocol_connect.go:325, 1060, 1089). Keying on the content type instead
// gets both ends wrong: a Connect GET carries none and would be missed, and
// any application/json POST would be claimed — so a browser webhook to a
// mistyped authority would get the Connect envelope instead of the
// human-readable body ADR 005 promises it.
//
// Both branches below require two markers rather than one, and for the same
// reason in each direction: a single marker that another kind of caller also
// happens to carry claims that caller, and hands a human a JSON envelope
// where ADR 005 promised a readable body.
//
// This inspects the request's framing, not its payload — voya still holds no
// descriptors and decodes nothing (ADR 003).
func isConnectUnary(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		// The GET form requires the codec alongside the version, so require
		// both. `connect` alone is an ordinary query parameter name, and a
		// browser URL that happens to carry ?connect=v1 is not a Connect
		// call — the same false positive an application/json content-type
		// check makes for POSTs.
		q := r.URL.Query()
		return q.Get("connect") == "v1" && q.Get("encoding") != ""

	case http.MethodPost:
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			return false
		}
		// Streaming Connect announces itself in the content type and reports
		// errors in an end-of-stream envelope, not as a JSON body. A content
		// type that is absent or does not parse is not a unary call we can
		// recognise either: guessing unary there would send a stream reader
		// a bare JSON object where it expects a length-prefixed frame,
		// turning the diagnostic into a parse error. Falling back to plain
		// text at least leaves a body someone can read.
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || strings.HasPrefix(mediaType, "application/connect+") {
			return false
		}
		return true

	default:
		return false
	}
}

// writeConnectError emits the Connect protocol's unary error envelope
// carrying the text the plain-text path would have written.
//
// The code is derived from the status the routing decision already chose,
// rather than fixed, because the difference is one a caller acts on. Every
// failure that names a service — never registered, declared but unbound,
// registered but unreachable — is unavailable, following gRPC's convention
// for a name that does not currently resolve to an endpoint: retrying it is
// at least meaningful. A request that arrived with no authority at all is
// different in kind. There is nothing to route on and no retry will supply
// one, so it is invalid_argument; reporting it as unavailable would put a
// caller with a retry policy into a loop it can never leave.
//
// It does still collapse the 404/503 distinction the plain-text path draws.
// For a Connect caller that distinction lives in the message text.
func writeConnectError(w http.ResponseWriter, status int, message string) {
	code, httpStatus := "unavailable", http.StatusServiceUnavailable
	if status == http.StatusBadRequest {
		code, httpStatus = "invalid_argument", http.StatusBadRequest
	}

	body, err := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
	if err != nil {
		http.Error(w, message, httpStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(body)
}

// quote renders a name for a message. Quoting matters more than it looks:
// trailing whitespace and invisible characters in an authority are exactly
// the kind of typo that reads as impossible until the quotes reveal it.
func quote(s string) string { return fmt.Sprintf("%q", s) }
