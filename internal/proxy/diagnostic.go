package proxy

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"strings"

	"github.com/trendvidia/voya/internal/registry"
)

// outcome is the routing decision ServeHTTP reached: why a request is not
// being proxied. diagnose takes the decision itself, and every status and
// code on the wire derives from it in [proxy.codeFor] — never the reverse.
// When the key was an HTTP status int, codeFor had to re-derive the decision
// and answered anything it did not recognise from a default, which is where
// 501 sat answering "try again" until #69; a status added at a future call
// site would have landed there the same way (#80).
//
// The type narrows that parameter without closing it, and the difference
// matters to anyone reading this as a guarantee. Go's untyped integer
// constants convert on assignment, so diagnose(w, r, http.StatusNotFound,
// …) — the shape every call site had before #80 — still compiles. What
// changed is where it lands: [proxy.codeFor]'s fallback, which answers
// internal and names the unmapped value on the log, rather than the old
// default's silent "try again". #91 weighed closing it at the type and
// declined: the candidate shapes cost iota and the dense sweep, or re-added
// the AST scan #80 removed.
type outcome int

const (
	// The request arrived with no :authority (HTTP/2) or Host (HTTP/1)
	// header at all, so there is nothing to route on.
	outcomeNoAuthority outcome = iota
	// The authority is not a name registry.ValidName can ever accept —
	// multi-label like api.payments.internal, not .internal at all, or a
	// single label no service may take (pay_ments.internal) — so the
	// request as sent can never come good: no registration, restart or
	// retry changes what the name is. outcomeNoAuthority's argument, one
	// header's difference (#88).
	outcomeNotAName
	// No service voya has ever seen answers to the name — a typo, or a
	// service that has never started. A name that got this far is one
	// registry.ValidName accepts, so it can register tomorrow, which is
	// what puts this on the retryable side of the line outcomeNotAName sits
	// on the other side of.
	outcomeNeverRegistered
	// The reserved voya.internal, which nothing can register, on a path
	// voya's own DiagService does not serve (ADR 005, #149).
	outcomeReserved
	// The service registered voya's own proxy port, so forwarding would
	// route voya to itself. The request is fine and the fleet's
	// registration is not: isSelf refuses it identically until the service
	// re-registers the port it actually bound, which is why its row is
	// failed_precondition rather than unavailable or invalid_argument
	// (#88; the split #80 kept it a distinct outcome for).
	outcomeRoutingLoop
	// Registered, but the connection to the registered port failed.
	outcomeUnreachable
	// The name registered before, and nothing is serving it now.
	outcomeKnownNotRunning
	// Live and routable, and over its rate_limit budget for the current
	// window (#223). The one outcome that reaches a caller with a
	// Retry-After header, because the wait is the diagnostic: the request
	// is fine and will be forwarded again when the window resets.
	outcomeRateLimited

	// numOutcomes bounds TestCodeForHasARowForEveryOutcome's sweep. It is
	// one past the last outcome and int rather than outcome, so it is not
	// itself a value diagnose accepts — the sentinel would otherwise be the
	// one outcome-typed constant codeFor has no row for. Keep it last, and
	// leave the block dense: an outcome given an explicit value would sit
	// outside the sweep's range and never be visited.
	numOutcomes int = iota
)

// diagnose is ADR 005's decision made concrete: the error response is the
// primary diagnostic surface. It names what arrived, says why it failed, and
// lists what would have worked — in the response the developer is already
// looking at, not in a log they have to think to check.
//
// Plain text is what a human reads through curl. JSON appears only inside the
// Connect error envelope, whose shape that protocol fixes rather than voya
// choosing it; ADR 003 keeps human-readable output elsewhere as pxf.
//
// Four shapes: gRPC and gRPC-Web read a header-carried status as the whole
// outcome, Connect streaming reads an end-of-stream envelope, Connect unary
// reads a JSON body, and everything else gets the text. Each detector
// requires two markers, so a caller none of them claims falls to plain text
// rather than being handed a frame it cannot parse.
//
// One edge of the order is load-bearing, and it is [isGRPC] before
// [isConnectUnary]: an application/grpc+proto POST that also carries
// Connect-Protocol-Version: 1 satisfies isConnectUnary's two markers, so
// asking it first would hand a gRPC client a JSON body. The two Connect
// detectors are mutually exclusive by construction instead — isConnectUnary
// refuses the application/connect+ prefix that isConnectStream requires — so
// their order between themselves decides nothing.
//
// What is deliberately *not* given a shape of its own, so the next reader
// does not take it for an oversight (#173):
//
//   - **gRPC-Web streaming** answers from [writeGRPCTrailersOnly] with the
//     rest of gRPC. A Trailers-Only response is a complete outcome for it —
//     the status rides the headers, which is exactly what that client reads
//     when a call fails before any message — so the body-framed trailers a
//     gRPC-Web *stream* would otherwise carry never need writing.
//   - **A Connect stream whose content type is absent or unparseable** stays
//     with plain text. [isConnectStream] will not claim it, for the reason
//     [isConnectUnary] gives in the other direction: guessing the framing
//     wrong turns the diagnostic into a parse error, and an unreadable
//     answer is worse than an unshaped one.
//   - **Connect GET** has no streaming form to claim; the protocol sends
//     only side-effect-free unary calls that way.
func (p *proxy) diagnose(w http.ResponseWriter, r *http.Request, oc outcome, reason string) {
	var b strings.Builder
	b.WriteString(reason)
	b.WriteString("\n\n")

	live := p.reg.Services()
	if len(live) > 0 {
		names := make([]string, 0, len(live))
		for _, up := range live {
			names = append(names, registry.Authority(up.Service))
		}
		fmt.Fprintf(&b, "Registered services: %s\n", strings.Join(names, ", "))
	} else {
		b.WriteString("Registered services: none — nothing has registered with voya yet.\n")
	}
	if idle := p.knownNotLive(); len(idle) > 0 {
		fmt.Fprintf(&b, "Known but not running: %s\n", strings.Join(idle, ", "))
	}

	out := p.codeFor(oc)
	switch {
	case isGRPC(r):
		writeGRPCTrailersOnly(w, r, out, b.String())
	case isConnectStream(r):
		writeConnectStreamError(w, r, out, b.String())
	case isConnectUnary(r):
		markVoyasOwn429(w, out.envelope)
		writeConnectError(w, out, b.String())
	default:
		markVoyasOwn429(w, out.routing)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(out.routing)
		fmt.Fprint(w, b.String())
	}
}

// markVoyasOwn429 stamps [RateLimitedHeader] on a response whose own status
// line is about to say 429. The marker means "this 429 is voya's own" — the
// limiter's, not an upstream's forwarded verdict (#241) — so it rides only
// the shapes where the status it qualifies is present: the plain-text
// response and the Connect unary envelope. The Trailers-Only and
// Connect-stream shapes answer HTTP 200 with the outcome in grpc-status or
// the end-of-stream envelope, so there is no 429 there for the marker to
// qualify and they carry Retry-After alone (#251, ADR 005). Keying on the
// status rather than the outcome keeps the header literal: whatever row
// writes a 429 is voya refusing on its own account — which is a rule about
// the write site, not about the shape. [writeConnectStreamError]'s two
// documented-unreachable escapes abandon the stream framing and answer under
// the envelope status, so they call this as well: a stream caller that ever
// does read a real 429 from voya reads a marked one.
func markVoyasOwn429(w http.ResponseWriter, status int) {
	if status == http.StatusTooManyRequests {
		w.Header().Set(RateLimitedHeader, "1")
	}
}

// knownNotLive is every name voya has seen that nothing is serving now.
func (p *proxy) knownNotLive() []string {
	var out []string
	for _, name := range p.reg.Known() {
		if _, live := p.reg.Lookup(name); !live {
			out = append(out, registry.Authority(name))
		}
	}
	return out
}

// isConnectUnary reports whether the request is a Connect unary call, which
// is the shape whose error envelope is plain JSON. Streaming Connect
// (application/connect+*) carries the outcome in an end-of-stream envelope
// instead; gRPC and gRPC-Web carry it in trailers, and [isGRPC] claims them
// before this is consulted.
//
// The discriminator is the protocol's own version marker rather than the
// content type. connect-go sets Connect-Protocol-Version: 1 on every Connect
// request, and for a side-effect-free unary call sent as GET it moves the
// marker to a connect=v1 query parameter and drops the content type entirely.
// Keying on content type instead gets both ends wrong: a Connect GET carries
// none and would be missed, and any application/json POST would be claimed —
// so a browser webhook to a mistyped authority would get a JSON envelope
// where ADR 005 promised it a readable body.
//
// Both branches require two markers rather than one, for the same reason in
// each direction: a single marker that another kind of caller also carries
// claims that caller.
//
// This inspects the request's framing, not its payload. voya still holds no
// descriptors and decodes nothing (ADR 003).
func isConnectUnary(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		// The GET form requires the codec alongside the version, so require
		// both: `connect` alone is an ordinary query parameter name, and a
		// browser URL that happens to carry ?connect=v1 is not a Connect
		// call.
		query := r.URL.Query()
		return query.Get("connect") == "v1" && query.Get("encoding") != ""

	case http.MethodPost:
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			return false
		}
		// Streaming Connect announces itself in the content type and reports
		// errors in an end-of-stream envelope, not as a JSON body. A content
		// type that is absent or unparseable is not a unary call we can
		// recognise either: guessing unary there hands a stream reader a
		// bare JSON object where it expects a length-prefixed frame, turning
		// the diagnostic into a parse error. Plain text at least leaves a
		// body someone can read.
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || strings.HasPrefix(mediaType, "application/connect+") {
			return false
		}
		return true

	default:
		return false
	}
}

// isConnectStream reports whether the request is a Connect *streaming* call,
// the shape [isConnectUnary] refuses. Its outcome rides an end-of-stream
// envelope in the body rather than a JSON object or a header, so it needs a
// writer of its own — [writeConnectStreamError] — and until #173 it fell
// through to plain text, which its client discards unread.
//
// Two markers again, and the same pair inverted: the protocol version says
// this is Connect at all, and the content type says which half of it. A
// content type that is absent or unparseable is not claimed here for the
// reason [isConnectUnary] gives in the other direction — guessing wrong
// hands a reader a frame it cannot parse, and plain text at least leaves
// something readable.
//
// The GET form has no streaming counterpart: Connect sends only
// side-effect-free unary calls that way, so there is nothing to claim.
//
// This inspects the request's framing, not its payload. voya still holds no
// descriptors and decodes nothing (ADR 003).
func isConnectStream(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Header.Get("Connect-Protocol-Version") != "1" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return strings.HasPrefix(mediaType, "application/connect+")
}

// isGRPC reports whether the request is a gRPC or gRPC-Web call, both of
// which carry a call's outcome in trailers — never in the HTTP status or an
// error body. A plain-text 404 is, to those clients, a transport failure with
// nothing attached, so they get a Trailers-Only response instead.
//
// Unlike Connect unary, the discriminator here is the content type:
// application/grpc* and application/grpc-web* mark nothing but these two
// protocols, and neither has a GET form for a content-type check to miss.
// Everything after the prefix is the codec (+proto, +json, -text's base64
// framing), which voya does not care about — it decodes nothing (ADR 003).
func isGRPC(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	rest, ok := strings.CutPrefix(mediaType, "application/grpc")
	if !ok {
		return false
	}
	if web, ok := strings.CutPrefix(rest, "-web"); ok {
		rest = strings.TrimPrefix(web, "-text")
	}
	return rest == "" || strings.HasPrefix(rest, "+")
}

// writeGRPCTrailersOnly answers a gRPC or gRPC-Web caller with the gRPC
// protocol's Trailers-Only shape: HTTP 200, the caller's content type in the
// form its own detector saw ([canonicalContentType], not the string it sent),
// grpc-status and grpc-message in the response headers, and no body. Over
// HTTP/2 that is a single HEADERS frame with END_STREAM — the shape
// test/trailer's TestTrailersOnlyError proves survives proxying; here voya
// originates one. Both protocols read a header-carried status as the whole
// outcome, gRPC-Web over HTTP/1.1 included, so one writer serves both.
//
// The codes come from [proxy.codeFor], once in diagnose, shared with
// writeConnectError.
func writeGRPCTrailersOnly(w http.ResponseWriter, r *http.Request, out codes, message string) {
	h := w.Header()
	h.Set("Content-Type", canonicalContentType(r))
	h.Set("Grpc-Status", out.grpc)
	h.Set("Grpc-Message", percentEncode(message))
	w.WriteHeader(http.StatusOK)
}

// percentEncode applies the gRPC wire protocol's encoding for grpc-message:
// '%' and every byte outside printable ASCII become %XX of their UTF-8 bytes.
// It is load-bearing, not cosmetic — the diagnostic is multi-line, and
// net/http will not carry a raw newline in a header value: over HTTP/2 it
// drops the whole field, and over HTTP/1.1 it rewrites each newline to a
// space. Unencoded, a gRPC caller would read an empty grpc-message and a
// gRPC-Web caller a flattened one, which is most of the diagnostic gone.
func percentEncode(message string) string {
	var b strings.Builder
	b.Grow(len(message))
	for i := range len(message) {
		c := message[i]
		if c < 0x20 || c > 0x7e || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// codes is one routing outcome's answer in every form voya writes it. The
// four fields are read by four different consumers — the plain-text
// response's own status, the grpc-status header, the Connect envelope's
// "code", and the HTTP status carrying that envelope — and they are one
// struct so the forms cannot disagree about what happened.
type codes struct {
	routing  int    // the plain-text response's own HTTP status
	grpc     string // the grpc-status header value
	connect  string // the Connect envelope's "code"
	envelope int    // the HTTP status carrying that envelope
}

// rowFor maps a routing outcome to everything the response writers say, and
// reports whether the outcome has a row at all. It is one function rather
// than a rule each writer applies, because the writers are obliged to agree
// and a shape repeated in two places is the one that drifts (#57) — and
// because #69 found the third row missing from both at once. ADR 005's table
// is this switch, row for row.
//
// Retryability is the axis that decides each row, because a code is read by
// a retry policy and not only by a human. The three unavailable rows can
// come good on their own — the developer restarts the process. The other
// four cannot by retrying: three never — no retry supplies an authority,
// repairs a name registry.ValidName can never accept, or makes the reserved
// name serve a procedure it does not have — and the routing loop not until a
// human fixes the fleet's registration, which is what failed_precondition
// tells a caller that unavailable and invalid_argument would each get wrong
// (#69, #88, ADR 005).
//
// The three unavailable rows collapse into one answer for a Connect or gRPC
// caller — one code, one envelope status — and keep their distinction in the
// routing status the plain-text path writes and in the message text, which
// names the port for the 502 and says "no running instance" for the 503.
//
// There is deliberately no default row. An outcome answered from a default
// is answered without anyone deciding, which is how 501 shipped as "try
// again" (#69); a switch over the closed set makes that a missing case
// instead, and TestCodeForHasARowForEveryOutcome sweeps the set asserting
// ok, so an outcome added with no row here fails in the test run of whoever
// added it (#80). What an outcome without a row answers on a live request is
// [proxy.codeFor]'s decision, not a row in the table.
func rowFor(oc outcome) (codes, bool) {
	switch oc {
	case outcomeNoAuthority:
		// Nothing to route on, and no retry will supply an authority.
		return codes{
			routing:  http.StatusBadRequest,
			grpc:     "3",
			connect:  "invalid_argument",
			envelope: http.StatusBadRequest,
		}, true
	case outcomeNotAName:
		// Not a name voya can ever route, and no retry of the request as
		// sent changes what the name is — outcomeNoAuthority's argument,
		// so its row (#88).
		return codes{
			routing:  http.StatusBadRequest,
			grpc:     "3",
			connect:  "invalid_argument",
			envelope: http.StatusBadRequest,
		}, true
	case outcomeNeverRegistered:
		return codes{
			routing:  http.StatusNotFound,
			grpc:     "14",
			connect:  "unavailable",
			envelope: http.StatusServiceUnavailable,
		}, true
	case outcomeReserved:
		// 501 both sides: Connect maps unimplemented to exactly the status
		// the routing decision already produced.
		return codes{
			routing:  http.StatusNotImplemented,
			grpc:     "12",
			connect:  "unimplemented",
			envelope: http.StatusNotImplemented,
		}, true
	case outcomeRoutingLoop:
		// The caller's request is fine; the fleet's registration is not,
		// and isSelf refuses it identically until a human fixes it. gRPC 9
		// is defined as "do not retry until the system state has been
		// explicitly fixed", and 400 is Connect's canonical status for the
		// code — the routing status derives from the code, never the
		// reverse (#88).
		return codes{
			routing:  http.StatusBadRequest,
			grpc:     "9",
			connect:  "failed_precondition",
			envelope: http.StatusBadRequest,
		}, true
	case outcomeUnreachable:
		return codes{
			routing:  http.StatusBadGateway,
			grpc:     "14",
			connect:  "unavailable",
			envelope: http.StatusServiceUnavailable,
		}, true
	case outcomeKnownNotRunning:
		return codes{
			routing:  http.StatusServiceUnavailable,
			grpc:     "14",
			connect:  "unavailable",
			envelope: http.StatusServiceUnavailable,
		}, true
	case outcomeRateLimited:
		// Retryable, and the only row that says when: the Retry-After
		// header serve sets before diagnosing is the answer's real content,
		// and honouring it is the caller bug ADR 006 built the limiter to
		// catch. gRPC 8 is resource_exhausted, and 429 is Connect's
		// canonical status for the code — the routing status derives from
		// the code, never the reverse (#88).
		return codes{
			routing:  http.StatusTooManyRequests,
			grpc:     "8",
			connect:  "resource_exhausted",
			envelope: http.StatusTooManyRequests,
		}, true
	}
	return codes{}, false
}

// codeFor is [rowFor] plus the answer for an outcome with no row, which is a
// bug in voya either way it happens: the set is closed by convention rather
// than by the compiler in both directions — the switch is not exhaustive to
// the compiler, and [outcome] admits any untyped integer constant, so a
// stale call site handing diagnose an http.Status* still compiles (#91).
// TestCodeForHasARowForEveryOutcome keeps the first direction unreachable;
// nothing keeps the second.
//
// The fallback answers rather than panicking, because a panic mid-handler
// reaches the caller as a reset stream with nothing attached — the one
// outcome ADR 005 exists to rule out. Loud means loud to the developer: the
// unmapped outcome is named on p.logf, and the caller still gets the
// diagnostic text. And it answers internal, not a retryable code: the
// failure is voya's own, and retryability is precisely what nobody decided,
// so "try again" would reopen the loop #69 closed (#91, ADR 005).
func (p *proxy) codeFor(oc outcome) codes {
	out, ok := rowFor(oc)
	if !ok {
		p.logf("voya: no code row for routing outcome %d — a bug in voya; answering internal", oc)
		return codes{
			routing:  http.StatusInternalServerError,
			grpc:     "13",
			connect:  "internal",
			envelope: http.StatusInternalServerError,
		}
	}
	return out
}

// writeConnectError emits the Connect protocol's unary error envelope
// carrying the text the plain-text path would have written. The codes come
// from [proxy.codeFor], once in diagnose, shared with writeGRPCTrailersOnly.
func writeConnectError(w http.ResponseWriter, out codes, message string) {
	body, err := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: out.connect, Message: message})
	if err != nil {
		// Unreachable for these two fields, but falling back to the readable
		// body beats writing nothing.
		http.Error(w, message, out.envelope)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.envelope)
	_, _ = w.Write(body)
}

// connectEndStreamFlag is the Connect protocol's end-of-stream bit in an
// envelope's flag byte (connect-go's connectFlagEnvelopeEndStream). The
// prefix it opens is five bytes: this one, then the payload length as a
// big-endian uint32.
const connectEndStreamFlag = 0x02

// writeConnectStreamError answers a Connect streaming caller with the
// protocol's end-of-stream envelope carrying the same text every other path
// writes: HTTP 200, the caller's content type canonicalised
// ([canonicalContentType]), then one frame — the flag
// byte, a four-byte big-endian length, and {"error":{"code":…,"message":…}}.
//
// The 200 is the whole point and not a formality. connect-go's streaming
// client validates the response status before it reads any of the body
// (connectStreamingClientConn.validateResponse), so a diagnostic written
// under the routing status is discarded unread and the caller is told only
// "unimplemented: HTTP status 404 Not Found" — ADR 005's primary surface,
// empty, on the one shape `grpcreflect`'s default client speaks (#173). The
// status a stream would otherwise carry is not lost: it is what
// [codes.connect] names in the envelope, which is where a stream reader
// looks.
//
// voya writes the frame by hand rather than borrowing connect-go's writer
// because it is answering a request it will not forward: there is no handler,
// no codec and no message here, only the pair [proxy.codeFor] already built.
// The framing is five bytes of it, and the payload is the same JSON shape
// [writeConnectError] marshals.
func writeConnectStreamError(w http.ResponseWriter, r *http.Request, out codes, message string) {
	body, err := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: out.connect, Message: message}})
	if err != nil {
		// Unreachable for these two fields. A stream reader cannot use a
		// plain-text body, but writing the status alone at least leaves the
		// caller the code it would have read. That status is the envelope's,
		// not the stream's 200, so the marker rides it: [markVoyasOwn429]
		// keys on what is written, and this row writes the 429.
		markVoyasOwn429(w, out.envelope)
		http.Error(w, message, out.envelope)
		return
	}

	// The payload length has to fit the frame's four bytes. A diagnostic is
	// a reason and one line per registered service, so this cannot bind —
	// but the conversion is the one place a wrong answer would be silent,
	// writing a truncated length and leaving the caller's reader
	// desynchronised rather than merely uninformed.
	//
	// Compared as uint64 because math.MaxUint32 is an untyped constant: as
	// written against an int it does not compile at all where int is 32 bits
	// (linux/386, linux/arm, windows/386), which is a target ADR 001's "runs
	// anywhere Go cross-compiles" covers and CROSS_TARGETS does not.
	if uint64(len(body)) > math.MaxUint32 {
		// The envelope status again, so the marker again — as above.
		markVoyasOwn429(w, out.envelope)
		http.Error(w, message, out.envelope)
		return
	}
	frame := make([]byte, 5, 5+len(body))
	frame[0] = connectEndStreamFlag
	//nolint:gosec // G115: bounded by the MaxUint32 check three lines up, which gosec does not follow.
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
	frame = append(frame, body...)

	// Canonicalised rather than echoed — see canonicalContentType for why,
	// and for what connect-go does with a verbatim echo.
	w.Header().Set("Content-Type", canonicalContentType(r))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame)
}

// canonicalContentType is the caller's content type as the answer's, in the
// form the caller's *detector* saw rather than the string it sent. The two
// ends read it differently: [isConnectStream] and [isGRPC] claim the request
// through mime.ParseMediaType, which lowercases the media type and drops its
// parameters, while the clients validate the *response* one more strictly —
// connect-go as a raw string, HasPrefix on "application/connect+" and then
// everything after that prefix compared to the codec the client asked for
// (connectValidateStreamResponseContentType). Echoed verbatim,
// "Application/Connect+Proto" or "application/connect+proto; charset=utf-8"
// is claimed here and then rejected there, and the diagnostic is lost
// exactly the way #173 lost it — for a caller one keystroke off canonical.
// gRPC clients read the response type more loosely, but it is the same
// divergence (#181), so both writers answer through this one function.
//
// An unparseable content type is left as it was: no detector claims one, so
// no caller of this function sees one, and there is nothing better to say.
func canonicalContentType(r *http.Request) string {
	contentType := r.Header.Get("Content-Type")
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		return mediaType
	}
	return contentType
}
