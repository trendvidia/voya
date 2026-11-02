package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	voyareflect "github.com/trendvidia/voya/client/reflection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/daemon"
)

// --- #158: voya call -----------------------------------------------------------
//
// The command is the only one that is a client of a *service* rather than of
// voya, so every test here runs the real binary against a real fleet: `voya
// run` on ports of its own, a fixture service joined through the client
// package, and `voya call …` in a process of its own against the same config
// directory. Nothing below stubs the schema, the proxy or the wire — the
// point of the command is that the developer's request takes the same route
// the fleet's own callers take.

// The fixture's schema. Built here as descriptors rather than added to
// proto/ and compiled: that directory is the fleet's schema surface, checked
// by `buf lint` and `buf breaking`, and a service that exists only to be
// called by a test does not belong in it. Everything this file hands the
// schema to takes descriptors anyway — grpcreflect serves them, -descriptors
// reads them back as a FileDescriptorSet, and pxf parses the request against
// them.
const (
	callFile    = "voya/calltest/v1/echo.proto"
	callPackage = "voya.calltest.v1"
	callService = callPackage + ".Echo"

	// The three procedures: one that answers, one that refuses, and one that
	// is a stream so `voya call` has something to refuse by name. Their
	// request and reply are the same one-string-field shape, which is all
	// the pxf in these tests needs.
	callEcho   = callService + "/Echo"
	callFail   = callService + "/Fail"
	callFollow = callService + "/Follow"

	// callWarmup is the path [waitRouted] knocks on, and no part of the
	// schema: a registration that has landed is what it establishes, and a
	// connect mux answers 404 to everything that is not a procedure.
	callWarmup = "/warmup"

	// callRefusal is the message the Fail procedure refuses with, and
	// callRefusalCode the code it carries.
	callRefusal     = "the fixture refuses this one on purpose"
	callRefusalCode = connect.CodeFailedPrecondition
)

// callSchema is the fixture's file as a FileDescriptorProto: two messages of
// one string field, and a service of three procedures over them.
func callSchema() *descriptorpb.FileDescriptorProto {
	text := func(name string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("text"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				JsonName: proto.String("text"),
			}},
		}
	}
	method := func(name string, streaming bool) *descriptorpb.MethodDescriptorProto {
		m := &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(name),
			InputType:  proto.String("." + callPackage + ".EchoRequest"),
			OutputType: proto.String("." + callPackage + ".EchoReply"),
		}
		if streaming {
			m.ServerStreaming = proto.Bool(true)
		}
		return m
	}
	return &descriptorpb.FileDescriptorProto{
		Name:        proto.String(callFile),
		Package:     proto.String(callPackage),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{text("EchoRequest"), text("EchoReply")},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Echo"),
			Method: []*descriptorpb.MethodDescriptorProto{
				method("Echo", false), method("Fail", false), method("Follow", true),
			},
		}},
	}
}

// callFiles is [callSchema] resolved, which is what a reflection server
// answers from and what a descriptor lookup reads.
func callFiles(t *testing.T) *protoregistry.Files {
	t.Helper()
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{callSchema()}})
	if err != nil {
		t.Fatalf("resolving the fixture schema: %v", err)
	}
	return files
}

// callMethodDesc is one procedure's descriptor, by the `Package.Service/Method`
// spelling the command takes.
func callMethodDesc(t *testing.T, procedure string) protoreflect.MethodDescriptor {
	t.Helper()
	svc, method, err := splitProcedure(procedure)
	if err != nil {
		t.Fatalf("%s: %v", procedure, err)
	}
	desc, err := callFiles(t).FindDescriptorByName(svc)
	if err != nil {
		t.Fatalf("%s: %v", svc, err)
	}
	md := desc.(protoreflect.ServiceDescriptor).Methods().ByName(method)
	if md == nil {
		t.Fatalf("the fixture schema has no %s", procedure)
	}
	return md
}

// callImage writes [callSchema] as the compiled FileDescriptorSet
// `-descriptors` reads — the same bytes `buf build -o` writes — and returns
// its path.
func callImage(t *testing.T) string {
	t.Helper()
	image, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{callSchema()}})
	if err != nil {
		t.Fatalf("marshalling the fixture image: %v", err)
	}
	path := filepath.Join(t.TempDir(), "echo.binpb")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("writing the fixture image: %v", err)
	}
	return path
}

// callFixture is the service `voya call` calls: the three procedures of
// [callSchema], optionally grpc.reflection.v1 over them, and a record of
// every path a request arrived on — which is how a test asserts that
// something was *not* sent.
type callFixture struct {
	mu    sync.Mutex
	paths []string
}

// calls is every request the fixture has been sent, less the harness's own
// warmup knocks. Recorded rather than filtered on the way in, so a test that
// asserts nothing was sent is reading the whole record and skipping the two
// paths it knows are not calls.
func (f *callFixture) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.paths {
		if p != callWarmup {
			out = append(out, p)
		}
	}
	return out
}

// reflected reports whether the fixture was asked for its schema, which is
// what tells a `-descriptors` call that skipped reflection from one that
// merely found it working.
func (f *callFixture) reflected() bool {
	for _, p := range f.calls() {
		if strings.Contains(p, "ServerReflection") {
			return true
		}
	}
	return false
}

// startCallFixture serves the fixture and registers it under name against
// v's daemon, and returns once the proxy routes to it. reflection is what
// ADR 004 (amended 2026-08-16) obliges a fleet service to serve; the fixture
// can be started without it, which is the case `-descriptors` exists for.
func startCallFixture(t *testing.T, v *voyaRun, name string, reflection bool) *callFixture {
	t.Helper()
	f := &callFixture{}

	mux := http.NewServeMux()
	mux.HandleFunc(callWarmup, func(http.ResponseWriter, *http.Request) {})
	mux.Handle("/"+callEcho, callHandler(t, callEcho, func(text string) (string, error) {
		return "echo: " + text, nil
	}))
	mux.Handle("/"+callFail, callHandler(t, callFail, func(string) (string, error) {
		return "", connect.NewError(callRefusalCode, errors.New(callRefusal))
	}))
	// No handler for callFollow. The schema is where `voya call` reads that
	// it is a stream, and the refusal is meant to come before anything is
	// dialled — a fixture that served it could not tell that refusal from a
	// call that went out and came back.
	if reflection {
		// The documented path (#174), not a second copy of it: whatever a
		// fleet service is told to write is what this fixture serves, so a
		// helper that stopped working would fail here rather than only in
		// somebody's own main.go. MountFrom because this schema is built
		// from a compiled image at run time rather than linked in.
		if err := voyareflect.MountFrom(mux, callFiles(t), callService); err != nil {
			t.Fatalf("mounting reflection: %v", err)
		}
	}

	serveAndJoin(t, v, name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	waitRouted(t, v, name+".internal")
	return f
}

// callHandler is one procedure of the fixture: a Connect unary handler over
// the same [dynamic] messages and [dynamicCodec] the command uses, since the
// fixture has no more Go types for its schema than the CLI does. answer maps
// the request's text to the reply's, or to the error the call fails with.
func callHandler(t *testing.T, procedure string, answer func(text string) (string, error)) http.Handler {
	t.Helper()
	md := callMethodDesc(t, procedure)
	in, out := md.Input(), md.Output()
	return connect.NewUnaryHandler("/"+procedure,
		func(_ context.Context, req *connect.Request[dynamic]) (*connect.Response[dynamic], error) {
			text, err := answer(req.Msg.msg.Get(in.Fields().ByName("text")).String())
			if err != nil {
				return nil, err
			}
			reply := dynamicpb.NewMessage(out)
			reply.Set(out.Fields().ByName("text"), protoreflect.ValueOfString(text))
			return connect.NewResponse(&dynamic{msg: reply}), nil
		},
		connect.WithCodec(&dynamicCodec{into: in}))
}

// startVoyaCall is [startVoyaCmd] for `voya call`.
func startVoyaCall(t *testing.T, v *voyaRun, args ...string) *voyaCmd {
	t.Helper()
	return startVoyaCmd(t, v, "call", args...)
}

// TestVoyaCallSendsPXFAndPrintsPXF is #158's first criterion: a fixture
// serving a unary procedure and reflection, and `voya call` reaching it by
// registered name with the request written as pxf — from the argument, and
// then the same request on stdin, which is the same call by another route.
// The reply is the pxf document and nothing else on stdout, so the output is
// pipeable; the schema came from the fixture's own reflection, which the
// record proves rather than assumes.
func TestVoyaCallSendsPXFAndPrintsPXF(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	f := startCallFixture(t, v, "fixture", true)

	const want = "text = \"echo: hi\"\n"

	fromArg := startVoyaCall(t, v, "fixture", callEcho, `text = "hi"`)
	if code := fromArg.exitCode(t, "against the fixture"); code != 0 {
		t.Fatalf("`voya call` exited %d\n%s", code, fromArg.Output())
	}
	if got := fromArg.Stdout.String(); got != want {
		t.Errorf("`voya call` printed %q on stdout, want %q", got, want)
	}
	if out := fromArg.Stderr.String(); out != "" {
		t.Errorf("`voya call` wrote to stderr: %q", out)
	}
	if !f.reflected() {
		t.Errorf("the fixture was never asked for its schema; it was sent %q", f.calls())
	}

	fromStdin := startVoyaCall(t, v, "fixture", callEcho)
	fromStdin.WriteStdin(t, `text = "hi"`)
	if code := fromStdin.exitCode(t, "with the request on stdin"); code != 0 {
		t.Fatalf("`voya call` with the request on stdin exited %d\n%s", code, fromStdin.Output())
	}
	if got := fromStdin.Stdout.String(); got != want {
		t.Errorf("`voya call` with the request on stdin printed %q, want %q", got, want)
	}
}

// TestVoyaCallReadsACompiledImage is `-descriptors` for a service that
// serves no reflection: the same reply, from the same fixture, with its
// reflection handlers left off. The setup is asserted rather than assumed —
// without the flag the same call fails, and says the fixture would not
// answer for its schema — because a passing `-descriptors` run against a
// fixture that was serving reflection all along would prove nothing.
func TestVoyaCallReadsACompiledImage(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	f := startCallFixture(t, v, "fixture", false)

	without := startVoyaCall(t, v, "fixture", callEcho, `text = "hi"`)
	if code := without.exitCode(t, "against a fixture serving no reflection"); code != 1 {
		t.Fatalf("`voya call` without -descriptors exited %d, want 1\n%s", code, without.Output())
	}
	if out := without.Stderr.String(); !strings.Contains(out, "schema") {
		t.Errorf("`voya call` without -descriptors said %q, want the schema named", out)
	}

	sent := len(f.calls())
	with := startVoyaCall(t, v, "-descriptors", callImage(t), "fixture", callEcho, `text = "hi"`)
	if code := with.exitCode(t, "with -descriptors"); code != 0 {
		t.Fatalf("`voya call -descriptors` exited %d\n%s", code, with.Output())
	}
	if got, want := with.Stdout.String(), "text = \"echo: hi\"\n"; got != want {
		t.Errorf("`voya call -descriptors` printed %q, want %q", got, want)
	}
	// The flag replaces the schema request rather than backing it up: what
	// this run sent is the call and nothing else.
	if got, want := f.calls()[sent:], []string{"/" + callEcho}; !slices.Equal(got, want) {
		t.Errorf("`voya call -descriptors` sent %q, want %q", got, want)
	}
}

// TestVoyaCallReportsTheCalleesRefusal: a procedure that answers with a
// Connect error is the callee speaking, so its code and its message reach
// stderr and the exit is 1 — with stdout empty, since there is no reply
// document and a reader of the pipe must not get prose instead of one.
func TestVoyaCallReportsTheCalleesRefusal(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	startCallFixture(t, v, "fixture", true)

	c := startVoyaCall(t, v, "fixture", callFail, `text = "hi"`)
	if code := c.exitCode(t, "against a procedure that refuses"); code != 1 {
		t.Fatalf("`voya call` on a refusal exited %d, want 1\n%s", code, c.Output())
	}
	out := c.Stderr.String()
	if !strings.Contains(out, callRefusalCode.String()) || !strings.Contains(out, callRefusal) {
		t.Errorf("`voya call` on a refusal said %q, want the code %q and the message %q",
			out, callRefusalCode, callRefusal)
	}
	if got := c.Stdout.String(); got != "" {
		t.Errorf("`voya call` on a refusal printed %q on stdout", got)
	}
}

// TestVoyaCallShowsVoyasDiagnostic: a name nothing has registered is voya's
// answer, not the callee's — ADR 005 makes the error response the primary
// diagnostic surface, so what `call` prints is that body, with the names
// that would have worked, and not a summary of its own.
func TestVoyaCallShowsVoyasDiagnostic(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	startCallFixture(t, v, "fixture", true)

	c := startVoyaCall(t, v, "absent", callEcho, `text = "hi"`)
	if code := c.exitCode(t, "against an unregistered name"); code != 1 {
		t.Fatalf("`voya call` against an unregistered name exited %d, want 1\n%s", code, c.Output())
	}
	out := c.Stderr.String()
	if !strings.Contains(out, voyaDiagnostic) || !strings.Contains(out, "absent.internal") ||
		!strings.Contains(out, "fixture.internal") {
		t.Errorf("`voya call` against an unregistered name said %q, want voya's diagnostic naming "+
			"absent.internal and the services that are registered", out)
	}
	// Said once. main prints every error as "voya: %v" and the body it is
	// handed already begins that way, so the diagnostic reaches the terminal
	// introducing voya twice unless [unprefixed] takes one off.
	if strings.Contains(out, voyaDiagnostic+voyaDiagnostic) {
		t.Errorf("`voya call` against an unregistered name said %q, want %q once, not around itself",
			out, voyaDiagnostic)
	}
	if got := c.Stdout.String(); got != "" {
		t.Errorf("`voya call` against an unregistered name printed %q on stdout", got)
	}
}

// TestVoyaCallRefusesAStreamingProcedure: unary only in #158, so a streaming
// procedure is refused by name and exit 2 — and refused before the procedure
// is sent anything, which the fixture's record is what shows (the schema
// request is there; a /Follow is not). The refusal is read out
// of the schema the fixture served, so this is also the descriptor being
// used for more than the message types.
func TestVoyaCallRefusesAStreamingProcedure(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	f := startCallFixture(t, v, "fixture", true)

	c := startVoyaCall(t, v, "fixture", callFollow, `text = "hi"`)
	if code := c.exitCode(t, "on a streaming procedure"); code != 2 {
		t.Fatalf("`voya call` on a streaming procedure exited %d, want 2\n%s", code, c.Output())
	}
	if out := c.Stderr.String(); !strings.Contains(out, callFollow) || !strings.Contains(out, "streaming") {
		t.Errorf("`voya call` on a streaming procedure said %q, want it refused by name", out)
	}
	for _, path := range f.calls() {
		if strings.HasSuffix(path, "/Follow") {
			t.Errorf("`voya call` sent the streaming procedure anyway: the fixture was sent %q", f.calls())
		}
	}
}

// TestVoyaCallRefusesAMalformedRequest: pxf that does not parse is the
// developer's typo, and the answer is the parser's own — the line it is on
// and the field it names — with nothing sent. `-descriptors` is what makes
// "nothing" the whole truth here: the schema comes off disk, so the fixture
// should see no request at all, not merely no call.
func TestVoyaCallRefusesAMalformedRequest(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	f := startCallFixture(t, v, "fixture", true)

	c := startVoyaCall(t, v, "-descriptors", callImage(t), "fixture", callEcho, `nosuch = "hi"`)
	if code := c.exitCode(t, "on a request that does not parse"); code != 1 {
		t.Fatalf("`voya call` on a malformed request exited %d, want 1\n%s", code, c.Output())
	}
	out := c.Stderr.String()
	if !strings.Contains(out, "nosuch") || !strings.Contains(out, "1:1") {
		t.Errorf("`voya call` on a malformed request said %q, want the field and its position", out)
	}
	if sent := f.calls(); len(sent) != 0 {
		t.Errorf("`voya call` sent %q before parsing the request", sent)
	}
}

// TestVoyaCallWhenVoyaIsNotRunning is the two answers `status` gives, from
// the command that reaches a service rather than the daemon: nothing on the
// proxy port is one line naming the port and `voya run`, exit 1, and a
// listener that accepts and never answers is the deadline line, exit 2. Both
// on stderr and not on stdout, unlike `status`, because stdout here is the
// reply document.
func TestVoyaCallWhenVoyaIsNotRunning(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	v.Signal(t, syscall.SIGTERM)
	v.WaitForExit(t, "after SIGTERM", shutdownBound)

	absent := startVoyaCall(t, v, "fixture", callEcho, `text = "hi"`)
	if code := absent.exitCode(t, "with no daemon"); code != 1 {
		t.Fatalf("`voya call` with no daemon exited %d, want 1\n%s", code, absent.Output())
	}
	if out := absent.Stderr.String(); !strings.Contains(out, "voya is not running on port") ||
		!strings.Contains(out, "voya run") {
		t.Errorf("`voya call` with no daemon said %q, want the port and what to run", out)
	}
	if got := absent.Stdout.String(); got != "" {
		t.Errorf("`voya call` with no daemon printed %q on stdout", got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.FormatUint(uint64(v.ports.Proxy), 10))
	if err != nil {
		t.Skipf("the stopped daemon's proxy port was taken in between: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	hung := startVoyaCall(t, v, "fixture", callEcho, `text = "hi"`)
	if code := hung.exitCode(t, "against a listener that never answers"); code != 2 {
		t.Fatalf("`voya call` against a listener that never answers exited %d, want 2\n%s", code, hung.Output())
	}
	if out := hung.Stderr.String(); !strings.Contains(out, "did not answer within") {
		t.Errorf("`voya call` against a listener that never answers said %q, want the deadline line", out)
	}
}

// TestSplitProcedure drives the one piece of parsing the command does before
// it dials anything. The shapes below are what a developer types when they
// have a `grpcurl` line in hand, or half of one.
func TestSplitProcedure(t *testing.T) {
	for _, tc := range []struct {
		procedure string
		svc       protoreflect.FullName
		method    protoreflect.Name
	}{
		{procedure: callEcho, svc: callService, method: "Echo"},
		{procedure: "voya.diag.v1.DiagService/GetStatus", svc: "voya.diag.v1.DiagService", method: "GetStatus"},
		// A service in no package: valid protobuf, and a shape a hand-written
		// .proto still produces.
		{procedure: "Echo/Whoami", svc: "Echo", method: "Whoami"},
		// The refusals: the Connect URL path, the gRPC spelling with the
		// leading slash, a dotted method, an empty half, and no separator.
		{procedure: "/" + callEcho},
		{procedure: callService + "/Echo/extra"},
		{procedure: callService + ".Echo"},
		{procedure: callService + "/"},
		{procedure: "/Echo"},
		{procedure: ""},
	} {
		t.Run(tc.procedure, func(t *testing.T) {
			svc, method, err := splitProcedure(tc.procedure)
			if tc.svc == "" {
				if err == nil {
					t.Fatalf("splitProcedure(%q) = %q, %q; want an error", tc.procedure, svc, method)
				}
				if !strings.Contains(err.Error(), "Package.Service/Method") {
					t.Errorf("splitProcedure(%q) said %q, want the shape it wanted", tc.procedure, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitProcedure(%q): %v", tc.procedure, err)
			}
			if svc != tc.svc || method != tc.method {
				t.Errorf("splitProcedure(%q) = %q, %q; want %q, %q", tc.procedure, svc, method, tc.svc, tc.method)
			}
		})
	}
}

// TestRequestText: the third word is the request, and its absence or a bare
// `-` means stdin. An empty argument is not an absence — it is the empty
// message — so it must not send the command reading a stdin nobody is
// writing to.
func TestRequestText(t *testing.T) {
	const fromStdin = `text = "piped"`
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "argument", args: []string{"fixture", callEcho, `text = "hi"`}, want: `text = "hi"`},
		{name: "empty argument", args: []string{"fixture", callEcho, ""}, want: ""},
		{name: "absent", args: []string{"fixture", callEcho}, want: fromStdin},
		{name: "dash", args: []string{"fixture", callEcho, "-"}, want: fromStdin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := stdin
			stdin = strings.NewReader(fromStdin)
			defer func() { stdin = restore }()
			got, err := requestText(tc.args)
			if err != nil {
				t.Fatalf("requestText(%q): %v", tc.args, err)
			}
			if string(got) != tc.want {
				t.Errorf("requestText(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestCallRejectsWhatItDoesNotTake drives the argument check through
// dispatch, without a daemon, since parsing comes before the dial: too few
// words, too many, an unknown flag, and -h as a request for help on stdout.
func TestCallRejectsWhatItDoesNotTake(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no words", args: []string{"call"}, want: "call takes a service"},
		{name: "one word", args: []string{"call", "fixture"}, want: "call takes a service"},
		{name: "four words", args: []string{"call", "fixture", callEcho, "text = 1", "more"}, want: "call takes a service"},
		{name: "unknown flag", args: []string{"call", "-nosuch", "fixture", callEcho}, want: "flag provided but not defined"},
		{name: "not a procedure", args: []string{"call", "fixture", "Echo.Whoami"}, want: "is not a procedure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("`voya %s` returned no error", strings.Join(tc.args, " "))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("`voya %s` failed with %q, want %q", strings.Join(tc.args, " "), err, tc.want)
			}
			if !strings.Contains(stderr.String(), "voya call [-descriptors FILE]") {
				t.Errorf("`voya %s` did not print call's usage on stderr:\n%s", strings.Join(tc.args, " "), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("`voya %s` wrote to stdout: %q", strings.Join(tc.args, " "), stdout.String())
			}
		})
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"call", "-h"}, &stdout, &stderr); err != nil {
		t.Fatalf("`voya call -h`: %v", err)
	}
	if !strings.Contains(stdout.String(), "voya call [-descriptors FILE]") {
		t.Errorf("`voya call -h` did not print usage on stdout:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("`voya call -h` wrote to stderr: %q", stderr.String())
	}
}

// TestCallImageErrors: `-descriptors` pointed at a file that is not there,
// and at one that is not a FileDescriptorSet. Both are the developer's own
// path, so both name it.
func TestCallImageErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nosuch.binpb")
	if _, err := filesFromImage(missing); err == nil {
		t.Errorf("filesFromImage(%q) read a file that is not there", missing)
	}
	junk := filepath.Join(t.TempDir(), "junk.binpb")
	if err := os.WriteFile(junk, []byte("this is not a descriptor set"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := filesFromImage(junk); err == nil || !strings.Contains(err.Error(), "FileDescriptorSet") {
		t.Errorf("filesFromImage on junk returned %v, want it named as not a FileDescriptorSet", err)
	}
	// A set without its import closure: what `protoc --descriptor_set_out`
	// writes when it is not also given --include_imports, and the one
	// failure of this file that is about how the file was made rather than
	// about the file. The hint is [imageHint]'s, and it keys on protodesc's
	// own wording — so this case failing is either that wording changing or
	// the hint going missing, and both want a reader.
	schema := callSchema()
	schema.Dependency = []string{"missing/dep.proto"}
	partial := filepath.Join(t.TempDir(), "partial.binpb")
	image, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{schema}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, image, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = filesFromImage(partial)
	if err == nil {
		t.Fatalf("filesFromImage(%q) resolved a set missing a file it imports", partial)
	}
	for _, want := range []string{"missing/dep.proto", "--include_imports"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("filesFromImage on a set without its closure said %q, want %q in it", err, want)
		}
	}
}

// TestVoyaCallReadsTheRequestBeforeItDials pins the order [callDeadline]'s
// clock depends on: the request comes off stdin before anything is dialled,
// so the ten seconds bound the round trip and not the developer typing. Read
// after the dial, a request that took longer to arrive than the deadline
// would end as [notRunning]'s deadline line — voya blamed, at exit 2, for a
// call nobody had made yet.
//
// A pipe nobody writes to is what shows the order, because it is the only
// thing here that can block: everything past the read is a dial to a port
// nothing holds, which is refused in microseconds. So a return before the
// write is the read having happened second. In process rather than as a
// child, since what is under test is an ordering rather than an exit code.
func TestVoyaCallReadsTheRequestBeforeItDials(t *testing.T) {
	ports, err := daemon.FreePorts(t.Context())
	if err != nil {
		t.Fatalf("drawing free ports: %v", err)
	}
	// A config directory of its own, for the reason startVoyaRun gives: the
	// developer's own voya on 9777 must not be what this dials.
	dir := t.TempDir()
	layer := fmt.Sprintf("proxy_port = %d\n", ports.Proxy)
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte(layer), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}
	t.Setenv(config.DirEnv, dir)

	r, w := io.Pipe()
	restore := stdin
	stdin = r
	t.Cleanup(func() { stdin = restore })

	done := make(chan error, 1)
	go func() { done <- callCmd([]string{"fixture", callEcho}, io.Discard, io.Discard) }()
	select {
	case err := <-done:
		t.Fatalf("`voya call` returned %v with its request still unwritten: it dialled before "+
			"reading stdin, so a slow request would spend the deadline", err)
	case <-time.After(500 * time.Millisecond):
	}

	// EOF is the empty message, and the rest of the command then runs into a
	// port nothing holds: the point is that it gets there at all.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("`voya call` succeeded against a port nothing holds")
		}
	case <-time.After(callDeadline + time.Second):
		t.Fatal("`voya call` never returned after its request arrived")
	}
}

// TestCallFailureSaysVoyaOnce: main prints "voya: %v" and every routing
// diagnostic internal/proxy writes begins with the same prefix, so the two
// meet in one line unless [callFailure] takes one off. A callee's own
// message is not voya's and is left exactly as it arrived.
func TestCallFailureSaysVoyaOnce(t *testing.T) {
	const diagnostic = "no service named \"absent\".\nRegistered services:\n  fixture"
	got := callFailure(9777, connect.NewError(connect.CodeUnavailable,
		errors.New(voyaDiagnostic+diagnostic))).Error()
	if want := connect.CodeUnavailable.String() + ": " + diagnostic; got != want {
		t.Errorf("callFailure on voya's diagnostic = %q, want %q", got, want)
	}
	got = callFailure(9777, connect.NewError(callRefusalCode, errors.New(callRefusal))).Error()
	if want := callRefusalCode.String() + ": " + callRefusal; got != want {
		t.Errorf("callFailure on the callee's refusal = %q, want %q", got, want)
	}
}

// TestMethodSchemaNamesTheProceduresThereAre: asking for a method the
// service does not have is answered with the ones it does, the way the proxy
// answers an unknown authority with the names that are registered (ADR 005).
func TestMethodSchemaNamesTheProceduresThereAre(t *testing.T) {
	_, err := methodSchema(t.Context(), http.DefaultClient, "http://unused.invalid",
		callImage(t), callService, "Nosuch")
	if err == nil {
		t.Fatal("methodSchema found a method the fixture schema does not have")
	}
	for _, want := range []string{"Nosuch", "Echo", "Fail", "Follow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("methodSchema said %q, want %q in it", err, want)
		}
	}
}

// TestUsageListsCallAsBuilt: the row is built, so usage lists it under
// Commands — the check [TestUsageListsStatusAndRoutesAsBuilt] makes for the
// M2 commands that came before it. The "not under Not implemented yet" half
// retired with the block itself (#210); TestEveryCommandIsBuilt in
// main_test.go pins the block gone.
func TestUsageListsCallAsBuilt(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"help"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !listsCommand(stdout.String(), "call") {
		t.Errorf("usage does not list `call` under Commands:\n%s", stdout.String())
	}
}
