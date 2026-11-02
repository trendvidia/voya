package acceptance

// --- #225: a Vanguard-served REST route through voya ---------------------------
//
// ADR 003 adopted connectrpc.com/vanguard in-process in each service and
// rejected a transcoding gateway: voya proxies bytes and holds no
// descriptors, so a REST URL should need nothing from voya at all. Its
// 2026-08-15 amendment says so plainly — "nothing in the binary" — and until
// this file that sentence had no evidence under it, because voya had never
// served a REST URL in a test.
//
// A REST call differs from every other request this suite makes in three
// ways at once, and each is asserted separately here: the method and shape
// (a GET with the message in the path and query, where everything else is a
// POST to a procedure path), the routing key (the same :authority ADR 001's
// dial convention exists for, carried in the URL like anything else), and
// the failure surface (a curl-shaped caller is the fourth shape ADR 005's
// diagnostic writers serve, and it falls to plain text by design — recorded
// in ADR 005's 2026-08-19 amendment, and pinned below).
//
// The fixture is deliberately not [echoHandler]: health and reflection are
// fleet obligations (ADR 004) and REST is not, so it gets a service of its
// own rather than every service in the suite growing a transcoder.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	voya "github.com/trendvidia/voya/client"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// restRoute is the google.api.http binding the fixture's one method carries.
// The {name} segment is what exercises Vanguard's routing skeleton rather
// than a bare literal path, and the /v1 prefix is ADR 006's
// forward-compatibility rule: REST paths carry a version from the first
// endpoint.
const restRoute = "/v1/greetings/{name}"

// restProcedure is the Connect procedure the route transcodes to — the path
// Vanguard's transcoder POSTs to [restBackend] after binding the URL to the
// request message.
const restProcedure = "/voya.acceptance.v1.Greetings/GetGreeting"

// restSchema is the fixture's descriptors, built in code for the reason
// [echoSchema] gives: proto/ is voya's own schema surface, and a service that
// exists to be called by tests does not belong in it. voya's own schemas are
// generated with buf and carry no @http; a fleet service's would come from
// `pxf build`, whose @http lowers to the same google.api.http extension set
// here directly (protowire#213, ADR 003's 2026-08-15 amendment).
//
// The import of google.golang.org/genproto/googleapis/api/annotations above
// is not just for the HttpRule literal — it is the type-registry obligation
// the amendment names: linking it is what registers the google.api.http
// extension, and a service without it fails as an empty route table rather
// than as an error, because Vanguard reads an extension it cannot resolve as
// no extension at all.
func restSchema() protoreflect.ServiceDescriptor {
	str := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(number),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}
	}
	opts := &descriptorpb.MethodOptions{}
	proto.SetExtension(opts, annotations.E_Http, &annotations.HttpRule{
		Pattern: &annotations.HttpRule_Get{Get: restRoute},
	})
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("voya/acceptance/v1/greetings.proto"),
		Package: proto.String("voya.acceptance.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("GetGreetingRequest"), Field: []*descriptorpb.FieldDescriptorProto{
				str("name", 1), str("flavor", 2),
			}},
			{Name: proto.String("Greeting"), Field: []*descriptorpb.FieldDescriptorProto{
				str("identity", 1), str("authority", 2), str("name", 3), str("flavor", 4),
			}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Greetings"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("GetGreeting"),
				InputType:  proto.String(".voya.acceptance.v1.GetGreetingRequest"),
				OutputType: proto.String(".voya.acceptance.v1.Greeting"),
				Options:    opts,
			}},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		// Not a t.Fatalf, for [echoSchema]'s reason: this builds the
		// fixture's own schema from constants in this file, so a failure is
		// this file being wrong for every test that serves the fixture.
		panic("building the REST fixture schema: " + err.Error())
	}
	svc := fd.Services().ByName("Greetings")
	if svc == nil {
		panic("the REST fixture schema lost its service")
	}
	return svc
}

// restHandler is the fixture's whole HTTP surface: Vanguard's transcoder in
// front of [restBackend], which is the one wrapper line ADR 003's amendment
// says a fleet service adds. Requests the transcoder does not rewrite — the
// Connect, gRPC and gRPC-Web shapes the backend already speaks — pass
// through untouched; a REST GET is bound to the request message and arrives
// at the backend as a Connect POST.
func restHandler(identity string) http.Handler {
	svc := restSchema()
	method := svc.Methods().ByName("GetGreeting")
	// The route-bound assertion #225 asks for, ahead of any call: the exact
	// failure protowire#213 caused was an @http annotation that stopped
	// lowering to google.api.http, which Vanguard reports as nothing — no
	// error, no route, every REST URL 404s. Checked here so a fixture whose
	// annotation went missing fails every REST test at construction, loudly
	// and by name, rather than as an unexplained 404. A panic rather than a
	// t.Fatalf, for the reason [echoHandler]'s reflection mount gives: the
	// cause is wiring visible in this file.
	if !method.Options().ProtoReflect().Has(annotations.E_Http.TypeDescriptor()) {
		panic("REST fixture: GetGreeting carries no google.api.http rule, so Vanguard would bind no routes " +
			"and every REST URL would 404 — the failure shape protowire#213 caused (ADR 003)")
	}
	transcoder, err := vanguard.NewTranscoder(
		[]*vanguard.Service{vanguard.NewServiceWithSchema(svc, restBackend(identity, method),
			// Bare proto and no compression, so the backend below reads one
			// shape. Without the codec pin the transcoder hands a REST
			// caller's JSON through unconverted — its default target codecs
			// include JSON — and a fleet service's generated connect-go
			// handler negotiates all of this itself and needs neither pin.
			vanguard.WithTargetCodecs(vanguard.CodecProto),
			vanguard.WithNoTargetCompression())},
	)
	if err != nil {
		panic("REST fixture: constructing the transcoder: " + err.Error())
	}
	return transcoder
}

// restBackend is the service half of the fixture: the Connect unary handler
// the transcoder targets. It is hand-rolled over dynamicpb rather than
// connect.NewUnaryHandler because the schema is dynamic — a fleet service
// has generated types and writes the one generated-handler line instead —
// and it answers with everything a test here needs to vouch for: the
// identity it was built with, the authority the request arrived on, and the
// two request fields, one bound from the path and one from the query.
func restBackend(identity string, method protoreflect.MethodDescriptor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != restProcedure {
			http.Error(w, "REST fixture: the transcoder targeted "+r.URL.Path+", want "+restProcedure, http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "REST fixture: reading the transcoded request: "+err.Error(), http.StatusInternalServerError)
			return
		}
		req := dynamicpb.NewMessage(method.Input())
		if err := proto.Unmarshal(body, req); err != nil {
			http.Error(w, "REST fixture: decoding the transcoded request: "+err.Error(), http.StatusInternalServerError)
			return
		}
		in, out := method.Input().Fields(), method.Output().Fields()
		reply := dynamicpb.NewMessage(method.Output())
		reply.Set(out.ByName("identity"), protoreflect.ValueOfString(identity))
		reply.Set(out.ByName("authority"), protoreflect.ValueOfString(r.Host))
		reply.Set(out.ByName("name"), req.Get(in.ByName("name")))
		reply.Set(out.ByName("flavor"), req.Get(in.ByName("flavor")))
		encoded, err := proto.Marshal(reply)
		if err != nil {
			http.Error(w, "REST fixture: encoding the reply: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		_, _ = w.Write(encoded)
	})
}

// startRESTService starts the fixture and registers it with voya under name,
// exactly as [startService] does for an echo service.
func startRESTService(t *testing.T, v *voyad, name, identity string) *service {
	t.Helper()
	return startHandlerOn(t, v.proxyPort, v.registrationPort, name, identity, restHandler(identity), func(port uint32) bool {
		up, live := v.Registry().Lookup(name)
		return live && up.Port == port
	})
}

// restResult is one REST answer, fully read: every assertion here is about
// the status, a header or the body, never about streaming one, so the helper
// drains and closes rather than handing each test a body to forget.
type restResult struct {
	status int
	header http.Header
	body   string
}

// restGET is one REST call through voya: a plain http.Client — no Connect
// client, no protocol options, the shape of a curl or a Postman — whose URL
// names the callee and whose only voya-specific part is the dialer, the same
// redirection every caller in this suite uses (ADR 002 as amended in #10).
func restGET(t *testing.T, proxyPort uint32, name, pathAndQuery string) restResult {
	t.Helper()
	return restDo(t, proxyPort, http.MethodGet, name, pathAndQuery)
}

func restDo(t *testing.T, proxyPort uint32, httpMethod, name, pathAndQuery string) restResult {
	t.Helper()
	transport := &http.Transport{
		Protocols:   httpProtocols(),
		DialContext: voya.Dialer(voya.WithProxyPort(proxyPort)),
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: callerTimeout}
	url := "http://" + name + ".internal:" + strconv.FormatUint(uint64(proxyPort), 10) + pathAndQuery
	req, err := http.NewRequest(httpMethod, url, nil)
	if err != nil {
		t.Fatalf("building %s %s: %v", httpMethod, url, err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", httpMethod, url, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the answer to %s %s: %v", httpMethod, url, err)
	}
	return restResult{status: res.StatusCode, header: res.Header, body: string(body)}
}

// greeting is the fixture's reply, decoded from the JSON Vanguard's REST
// protocol writes.
type greeting struct {
	Identity  string `json:"identity"`
	Authority string `json:"authority"`
	Name      string `json:"name"`
	Flavor    string `json:"flavor"`
}

func decodeGreeting(t *testing.T, res restResult) greeting {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("REST call answered %d, want 200; body:\n%s", res.status, res.body)
	}
	var g greeting
	if err := json.Unmarshal([]byte(res.body), &g); err != nil {
		t.Fatalf("the REST reply is not the JSON Vanguard writes: %v; body:\n%s", err, res.body)
	}
	return g
}

// TestARESTRouteIsServedThroughVoya is #225's headline: a GET with the
// message in the path, through voya's proxy, answered by a Vanguard-bound
// route in the service — and nothing in voya knows any of that happened. The
// routing key is the claim: rest.internal in the URL, the same dial string
// every POST in this suite carries, and the callee sees it arrive as its
// authority.
func TestARESTRouteIsServedThroughVoya(t *testing.T) {
	v := startVoya(t)
	startRESTService(t, v, "rest", "rest/only")

	// The route is bound, asserted independently of the GET below answering:
	// a request whose method the route does not serve is refused *by the
	// route* — 405 naming GET — where an unbound path answers 404. So a
	// transcoder that silently bound nothing (the protowire#213 shape, which
	// [restHandler] also refuses at construction) fails here with "404, want
	// 405" rather than the GET failing with an answer that could equally be
	// a routing bug in voya.
	if res := restDo(t, v.proxyPort, http.MethodDelete, "rest", "/v1/greetings/alice"); res.status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE on the bound path answered %d, want 405 from the route table", res.status)
	} else if allow := res.header.Get("Allow"); allow != http.MethodGet {
		t.Errorf("the 405's Allow header is %q, want %q", allow, http.MethodGet)
	}

	got := decodeGreeting(t, restGET(t, v.proxyPort, "rest", "/v1/greetings/alice"))
	if got.Identity != "rest/only" {
		t.Errorf("the REST call reached %q, want rest/only", got.Identity)
	}
	if got.Name != "alice" {
		t.Errorf("the {name} segment bound %q, want alice", got.Name)
	}
	// The name it was dialled by, not the address it was routed to — the
	// same assertion TestTheSkeletonWalks makes for a Connect caller, and
	// the reason a REST URL needs nothing from voya: the authority is the
	// routing key whatever the method and path around it say.
	if !strings.Contains(got.Authority, "rest.internal") {
		t.Errorf("the callee saw authority %q, want rest.internal", got.Authority)
	}
}

// TestARESTQueryParameterArrivesAndStaysOutOfTheLog: Vanguard binds a request
// field the path template does not name from the query string, so a REST
// call is the first thing in this suite where ADR 005's "the path alone,
// never the query" carries payload rather than protocol plumbing. Both
// halves are asserted: the parameter reaches the handler, and the request
// log's row — read through DiagService like any other consumer — records
// GET, the path with no trace of it, the authority, and the upstream port,
// the way a developer expects a REST call to read.
func TestARESTQueryParameterArrivesAndStaysOutOfTheLog(t *testing.T) {
	v := startVoya(t)
	svc := startRESTService(t, v, "rest", "rest/only")

	got := decodeGreeting(t, restGET(t, v.proxyPort, "rest", "/v1/greetings/alice?flavor=tea"))
	if got.Flavor != "tea" {
		t.Errorf("the query parameter bound %q, want tea", got.Flavor)
	}

	// The record lands after the response is written; the caller can be
	// ahead of it by a scheduling quantum, so the read polls.
	c := diagClient(t, v.proxyPort)
	var row *diagv1.Request
	waitFor(t, "the REST call to be recorded", func() bool {
		res, err := c.ListRequests(t.Context(), connect.NewRequest(&diagv1.ListRequestsRequest{}))
		if err != nil {
			return false
		}
		for _, r := range res.Msg.GetRequests() {
			if r.GetAuthority() == "rest.internal" {
				row = r
				return true
			}
		}
		return false
	})
	if row.GetMethod() != http.MethodGet || row.GetPath() != "/v1/greetings/alice" ||
		row.GetStatus() != http.StatusOK || row.GetUpstreamPort() != svc.port() {
		t.Errorf("recorded %v, want GET /v1/greetings/alice 200 -> %d", row, svc.port())
	}
	if strings.Contains(row.GetPath(), "flavor") || strings.Contains(row.GetPath(), "?") {
		t.Errorf("the recorded path %q carries the query string, which is payload (ADR 005)", row.GetPath())
	}
}

// TestARESTCallToAGoneInstanceIsExplained is the eviction path from the
// fourth caller shape: the name is known, nothing serves it, and a caller
// that is not Connect gets ADR 005's plain-text diagnostic with the
// known-not-running row's own status — 503, where a Connect caller's
// envelope collapses the three unavailable rows together.
func TestARESTCallToAGoneInstanceIsExplained(t *testing.T) {
	v := startVoya(t)
	callee := startRESTService(t, v, "rest", "rest/doomed")

	if res := restGET(t, v.proxyPort, "rest", "/v1/greetings/alice"); res.status != http.StatusOK {
		t.Fatalf("the call before the shutdown answered %d", res.status)
	}

	callee.shutdown()
	waitFor(t, "the vanished service to be evicted", func() bool {
		_, live := v.Registry().Lookup("rest")
		return !live
	})

	res := restGET(t, v.proxyPort, "rest", "/v1/greetings/alice")
	assertRESTDiagnostic(t, res, http.StatusServiceUnavailable, "rest")
}

// TestARESTCallToAnUnregisteredNameReadsAsPlainText pins the decision #225
// asked for, recorded in ADR 005's 2026-08-19 amendment: a REST caller —
// curl, Postman, a browser URL bar — carries none of the markers the three
// protocol-shaped diagnostic writers key on, so it falls to plain text, and
// that fall-through is the right answer rather than a gap. The table gains
// no row; this test is what notices if a future detector starts claiming
// the shape.
func TestARESTCallToAnUnregisteredNameReadsAsPlainText(t *testing.T) {
	v := startVoya(t)
	startRESTService(t, v, "rest", "rest/only")

	res := restGET(t, v.proxyPort, "nonesuch", "/v1/greetings/alice")
	assertRESTDiagnostic(t, res, http.StatusNotFound, "nonesuch")
	// What a human reads, not an envelope: the never-registered reason and
	// the pointer to what would have worked.
	if !strings.Contains(res.body, "Nothing has ever registered that name") {
		t.Errorf("the diagnostic lost its reason:\n%s", res.body)
	}
	if !strings.Contains(res.body, "rest.internal") {
		t.Errorf("the diagnostic does not list the registered service that would have worked:\n%s", res.body)
	}
}

// TestARESTQueryNamedLikeAConnectGETIsClaimedAsConnect is the one exception
// to the test above, and ADR 005's amendment names it rather than leaving a
// reader to find it. The Connect GET detector keys on two query parameters,
// connect=v1 beside a non-empty encoding (internal/proxy/diagnostic.go), and
// a route template is free to bind request fields of exactly those names: at
// the framing voya inspects, such a URL *is* a Connect GET, so voya claims it
// and writes the envelope — the 503 that collapses the three unavailable
// rows, where the same call without the pair reads 404 in plain text.
//
// Telling the two apart would need the descriptors ADR 003 keeps out of voya,
// so this is a documented divergence rather than a gap to close. The test is
// what makes a future change to either detector face it.
func TestARESTQueryNamedLikeAConnectGETIsClaimedAsConnect(t *testing.T) {
	v := startVoya(t)
	startRESTService(t, v, "rest", "rest/only")

	// The control, and the thing that makes the difference below about the
	// query pair rather than about REST: an ordinary bound query parameter
	// leaves the fall-through exactly as the test above found it.
	plain := restGET(t, v.proxyPort, "nonesuch", "/v1/greetings/alice?flavor=warm")
	assertRESTDiagnostic(t, plain, http.StatusNotFound, "nonesuch")

	claimed := restGET(t, v.proxyPort, "nonesuch", "/v1/greetings/alice?connect=v1&encoding=json")
	if claimed.status != http.StatusServiceUnavailable {
		t.Errorf("the claimed REST call answered %d, want the envelope's 503; body:\n%s", claimed.status, claimed.body)
	}
	if ct := claimed.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("the claimed REST call's content type is %q, want application/json", ct)
	}
	if !strings.Contains(claimed.body, `"code":"unavailable"`) {
		t.Errorf("the claimed REST call did not get the Connect error envelope:\n%s", claimed.body)
	}
}

// assertRESTDiagnostic holds a failed REST call to what ADR 005 promises the
// fourth caller shape: the routing status as the response's own — the one
// shape that keeps the three unavailable rows distinct — a plain-text
// content type rather than a Connect envelope, and a body that is voya
// explaining itself by name.
func assertRESTDiagnostic(t *testing.T, res restResult, wantStatus int, service string) {
	t.Helper()
	if res.status != wantStatus {
		t.Errorf("the diagnostic's status is %d, want %d; body:\n%s", res.status, wantStatus, res.body)
	}
	if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("the diagnostic's content type is %q, want text/plain — a REST caller reads text, not an envelope", ct)
	}
	// The "voya:" prefix, for [assertVoyaDiagnostic]'s reason: the service
	// name alone is also in every transport failure's URL.
	if !strings.Contains(res.body, "voya:") || !strings.Contains(res.body, service) {
		t.Errorf("the body is not voya's diagnostic about %s:\n%s", service, res.body)
	}
}

// TestHEADAndUnsupportedMethodsOnABoundRESTPath pins what the route table
// answers around the one method it serves. Vanguard matches methods exactly
// — a HEAD is not a GET — so both a HEAD and a DELETE on the bound path get
// 405 with Allow naming what would work, and a GET on a path no rule binds
// gets the service's own 404, distinguishable from voya's by carrying no
// diagnostic: voya routed it, the service answered it, exactly the division
// ADR 003 draws.
func TestHEADAndUnsupportedMethodsOnABoundRESTPath(t *testing.T) {
	v := startVoya(t)
	startRESTService(t, v, "rest", "rest/only")

	for _, httpMethod := range []string{http.MethodHead, http.MethodDelete} {
		res := restDo(t, v.proxyPort, httpMethod, "rest", "/v1/greetings/alice")
		if res.status != http.StatusMethodNotAllowed {
			t.Errorf("%s on the bound path answered %d, want 405", httpMethod, res.status)
		}
		if allow := res.header.Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s's Allow header is %q, want %q", httpMethod, allow, http.MethodGet)
		}
	}

	res := restGET(t, v.proxyPort, "rest", "/v1/nonesuch")
	if res.status != http.StatusNotFound {
		t.Errorf("a GET on an unbound path answered %d, want the service's own 404", res.status)
	}
	if strings.Contains(res.body, "voya:") {
		t.Errorf("the unbound path's 404 reads as voya's, so the request never reached the service:\n%s", res.body)
	}
}
