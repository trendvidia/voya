package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/trendvidia/voya/internal/registry"
)

// callDeadline bounds the whole of one `voya call` — the schema and the call
// — because both legs are the same round trip to the same place and a
// developer waiting at a prompt is waiting for the reply, not for a phase.
//
// Longer than the deadlines the DiagService commands carry, and for the
// reason [healthDeadline] gives at two seconds: what answers here is a
// developer's own service doing whatever the procedure does, not voya
// reading its memory, and a service that takes a second to answer a hand-
// typed poke is working rather than wedged. What the bound buys is the same
// thing [diagDeadline] buys — `voya call … | …` against a wedged voya ends
// with a sentence and exit 2 rather than hanging — so it stays short enough
// that a developer reaches for ^C after it, not before.
//
// The clock starts once the request is in hand, which is why [callCmd] reads
// it before opening this context: a request arriving on stdin is a developer
// typing or a producer upstream in a pipeline, and neither is voya being
// slow. Bounding that too would answer a request that took eleven seconds to
// type with [notRunning]'s "did not answer within 10s" — voya blamed, at exit
// 2, for a call that was never made.
const callDeadline = 10 * time.Second

// stdin is where `voya call` reads a request it was not given as an
// argument. A variable so a test can hand it a string: [command.run] takes
// the two output streams and no input one, since `call` is the only command
// that reads any.
var stdin io.Reader = os.Stdin

// callCmd is `voya call` (#158): ADR 003's Consequences made a command —
// "for command-line poking at internal services, `voya call` should speak
// protowire and render pxf rather than pushing developers toward the JSON
// path". The request is written as pxf and the reply printed as pxf, with
// binary protobuf on the wire between them, through the proxy under the
// callee's own authority — so a working call is also the fleet's own routing
// working, and nothing here has taught a developer a JSON path ADR 003 keeps
// for browsers.
//
// voya holds no descriptors and gains none (ADR 003): this process fetches
// the callee's schema at call time, uses it, and forgets it. Reflection is
// the source — `grpc.reflection.v1` on the callee, reached the way every
// other caller reaches it, which is what makes authority routing resolve
// reflection per service — and -descriptors is the fallback for a service
// that serves none. Serving reflection is a fleet obligation beside
// `grpc.health.v1` (ADR 004, amended 2026-08-16).
//
// Streaming procedures are refused by name, out of the schema and before the
// procedure is sent anything: this command is unary only, and a stream
// half-rendered as pxf would be a worse answer than a refusal that says so.
//
// stdout carries the reply document and nothing else, so that `voya call … |
// …` hands its reader pxf rather than pxf with a sentence in it. Everything
// else — a Connect error's code and message, voya's routing diagnostic, the
// two answers a voya that is absent or not answering gives — goes to stderr
// with the verdict in the exit code: 0 an answer, 2 a refusal or a voya that
// never answered, 1 everything else.
func callCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("voya call", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	image := fs.String("descriptors", "",
		"read the schema from this compiled FileDescriptorSet instead of asking the service")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			callUsage(stdout)
			return nil
		}
		callUsage(stderr)
		return err
	}
	if fs.NArg() < 2 || fs.NArg() > 3 {
		callUsage(stderr)
		return fmt.Errorf("call takes a service, a procedure and an optional request, got %q",
			strings.Join(fs.Args(), " "))
	}
	// The service name is not checked here. An authority no service may take
	// is the proxy's diagnosis to make and it makes it well (ADR 005): a
	// second copy of registry.ValidName in the CLI would answer first, in
	// words the developer would then not find when curl gets the same name
	// wrong.
	service, procedure := fs.Arg(0), fs.Arg(1)
	svc, method, err := splitProcedure(procedure)
	if err != nil {
		callUsage(stderr)
		return err
	}

	// Before the deadline opens, for the reason [callDeadline] gives: this
	// may block on a person typing, and their typing is not voya failing to
	// answer. Nothing has been dialled yet either, so a request that never
	// ends holds no connection open.
	text, err := requestText(fs.Args())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
	defer cancel()
	port, transport, err := dialProxy(ctx)
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	base := "http://" + registry.Authority(service) + ":" + strconv.FormatUint(uint64(port), 10)

	md, err := methodSchema(ctx, httpClient, base, *image, svc, method)
	if err != nil {
		return callFailure(port, err)
	}
	if md.IsStreamingClient() || md.IsStreamingServer() {
		return &exitStatus{code: 2, err: fmt.Errorf(
			"%s is a streaming procedure, and `voya call` does not support streaming yet", procedure)}
	}

	// Parsed against the callee's own request descriptor, and before the
	// call: a request that does not parse is the developer's typo, and pxf
	// names the line and the field it is on, which is a better answer than
	// whatever a service makes of a message it did not expect. Nothing is
	// sent when this fails.
	req, err := pxf.UnmarshalDescriptor(text, md.Input())
	if err != nil {
		return fmt.Errorf("the request is not %s: %w", md.Input().FullName(), err)
	}

	// Connect unary, binary protobuf codec, Content-Type: application/proto
	// — ADR 003's wire between our own components. [dynamicCodec] is what
	// makes that possible without a descriptor compiled into this binary,
	// and connect's own error mapping is what turns the callee's refusal and
	// voya's diagnostic alike into a *connect.Error below.
	client := connect.NewClient[dynamic, dynamic](httpClient, base+"/"+string(svc)+"/"+string(method),
		connect.WithCodec(&dynamicCodec{into: md.Output()}))
	res, err := client.CallUnary(ctx, connect.NewRequest(&dynamic{msg: req}))
	if err != nil {
		return callFailure(port, err)
	}
	return pxfDocument(stdout, res.Msg.msg)
}

// splitProcedure splits `Package.Service/Method` into its two halves, which
// is how gRPC and Connect both spell a procedure and so what a developer
// already has in front of them — a `grpcurl` line, a Connect URL, an ADR.
// The check is here rather than left to the first thing that fails on it
// because both consumers, the reflection lookup and the URL, would otherwise
// report a typo as the callee's answer to a question it was never asked.
func splitProcedure(procedure string) (protoreflect.FullName, protoreflect.Name, error) {
	svc, method, ok := strings.Cut(procedure, "/")
	full, name := protoreflect.FullName(svc), protoreflect.Name(method)
	if !ok || strings.Contains(method, "/") || !full.IsValid() || !name.IsValid() {
		return "", "", fmt.Errorf("%q is not a procedure: write Package.Service/Method, as in voya.diag.v1.DiagService/GetStatus", procedure)
	}
	return full, name, nil
}

// requestText is the pxf request: the third word when there is one, and
// stdin when there is not or when it is `-`, so a request too long or too
// quoted for a shell word can be piped or heredoc'd in. An empty request is
// not missing — it is the empty message, which is what a procedure taking no
// arguments is called with.
//
// A procedure taking none is therefore called with an empty word — two
// quotes in the shell — and not with no word at all: no word means stdin,
// and at a terminal stdin is the developer, until they end it with ^D.
// Nothing here guesses that a terminal meant the empty message, because a
// bare `-` and an interactive request are the same thing spelled two ways
// and one of them would stop working; #158 says stdin when the word is
// absent, and usage and README say to write the empty word.
func requestText(args []string) ([]byte, error) {
	if len(args) > 2 && args[2] != "-" {
		return []byte(args[2]), nil
	}
	text, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("reading the request from stdin: %w", err)
	}
	return text, nil
}

// methodSchema is the callee's descriptor for one procedure: from the
// compiled FileDescriptorSet at image when there is one, and from the
// service's own `grpc.reflection.v1` when there is not.
//
// The lookup asks for the service and finds the method inside it, rather
// than asking for the method: every reflection server resolves a service
// name, the method is one field lookup away once the file is in hand, and a
// method that is not there can then be reported with the ones that are.
func methodSchema(ctx context.Context, httpClient connect.HTTPClient, base, image string,
	svc protoreflect.FullName, method protoreflect.Name,
) (protoreflect.MethodDescriptor, error) {
	var (
		files *protoregistry.Files
		err   error
	)
	if image != "" {
		files, err = filesFromImage(image)
	} else {
		files, err = filesFromReflection(ctx, httpClient, base, svc)
	}
	if err != nil {
		return nil, err
	}
	desc, err := files.FindDescriptorByName(svc)
	if err != nil {
		return nil, fmt.Errorf("the schema has no %s: %w", svc, err)
	}
	sd, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a service", svc)
	}
	md := sd.Methods().ByName(method)
	if md == nil {
		return nil, fmt.Errorf("%s has no method %s; it has %s", svc, method, methodList(sd))
	}
	return md, nil
}

// methodList names every procedure a service has, for the message that says
// the one asked for is not among them — the same shape as the proxy's
// "Registered services:" line, which is ADR 005's habit of answering a wrong
// name with the right ones.
func methodList(sd protoreflect.ServiceDescriptor) string {
	names := make([]string, 0, sd.Methods().Len())
	for i := range sd.Methods().Len() {
		names = append(names, string(sd.Methods().Get(i).Name()))
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// filesFromImage reads a serialized FileDescriptorSet — what `buf build -o`
// and `protoc --descriptor_set_out` write — for a callee that serves no
// reflection. The set must carry the whole closure, imports included: `buf
// build` writes one that does, and `protoc` writes the named files alone
// unless it is also given --include_imports, which is why the failure that
// leaves says so. Reflection has no such edge — a server answers
// FileContainingSymbol with the closure — so this is the image path's alone.
func filesFromImage(path string) (*protoregistry.Files, error) {
	// The path is the developer's own argument to their own command, which
	// is the one place a file name is not a way in for anything.
	image, err := os.ReadFile(path) //nolint:gosec // G304: the path is this command's own -descriptors argument
	if err != nil {
		return nil, fmt.Errorf("reading -descriptors: %w", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(image, &set); err != nil {
		return nil, fmt.Errorf("%s is not a compiled FileDescriptorSet: %w", path, err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("%s: %w%s", path, err, imageHint(err))
	}
	return files, nil
}

// imageHint is the sentence that follows a set missing a file it imports,
// naming the flag that would have put it there. Only for that failure: a set
// that is malformed some other way has nothing to do with --include_imports,
// and everything protodesc reports about an import it could not resolve
// names the import rather than the reason there isn't one.
//
// Matched on protodesc's own words, since the package returns a plain error
// for this and there is nothing else to key on. TestCallImageErrors builds a
// set with a dangling import, so a rewording upstream fails a test here
// rather than quietly dropping the hint.
func imageHint(err error) string {
	if !strings.Contains(err.Error(), "could not resolve import") {
		return ""
	}
	return "\n\nA compiled set must carry the whole import closure. " +
		"`buf build -o` writes one; protoc needs --include_imports."
}

// filesFromReflection asks the callee itself, over `grpc.reflection.v1` on
// the same authority and port the call will use — through voya, so the
// schema comes from the instance the request will reach rather than from
// whatever else answers that name elsewhere.
//
// One symbol on a fresh stream, so the answer is the file plus its whole
// dependency closure: a server may omit what it has already sent on a
// stream, and this stream has been sent nothing.
func filesFromReflection(ctx context.Context, httpClient connect.HTTPClient, base string,
	svc protoreflect.FullName,
) (*protoregistry.Files, error) {
	stream := grpcreflect.NewClient(httpClient, base).NewStream(ctx)
	defer func() { _, _ = stream.Close() }()
	fds, err := stream.FileContainingSymbol(svc)
	if err != nil {
		// voya's own answer, when this never reached a service at all, is
		// what the developer needs and all of what they need — so it is the
		// whole message, not a line of this command's framing wrapped around
		// a routing failure that says the name, the names that would have
		// worked, and why.
		// Since #173 voya answers a Connect stream — which is what
		// reflection is — with an end-of-stream error envelope, so its
		// diagnostic arrives as this error's own message rather than as a
		// plain-text body connect-go would have dropped. The prefix is what
		// tells voya's answer from a service's own refusal.
		var cerr *connect.Error
		if errors.As(err, &cerr) && strings.HasPrefix(cerr.Message(), voyaDiagnostic) {
			return nil, errors.New(unprefixed(cerr.Message()))
		}
		// Wrapped, so that [notRunning] still finds a refused connection or
		// an expired deadline underneath — this leg is the one that meets an
		// absent voya first. What the wrap costs is that the message is no
		// longer a bare *connect.Error, which is why [callFailure] collapses
		// only an unwrapped one.
		return nil, fmt.Errorf("asking %s for the schema of %s: %w%s", base, svc, err, reflectionHint(err))
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: fds})
	if err != nil {
		return nil, fmt.Errorf("the schema %s served for %s: %w", base, svc, err)
	}
	return files, nil
}

// callFailure turns a failure of either leg — the schema stream or the call
// — into what the command prints and the code it exits with. The two
// not-a-failure answers are [notRunning]'s, shared with the DiagService
// commands so that a voya that is absent or wedged reads the same whichever
// command found it.
//
// Everything else is exit 1, and a *connect.Error is rendered as its code
// and its message: for a callee that refused, the message is the callee's
// own, and for a name voya could not route, it is voya's routing diagnostic
// — the primary diagnostic surface of ADR 005, which `call` therefore shows
// rather than replacing with a summary of its own.
//
// The type assertion is deliberate where the rest of this file reaches for
// errors.As: an unwrapped *connect.Error is the callee having answered, and
// its code and message are then the whole story. A wrapped one came from
// this command's own framing — [filesFromReflection]'s, which says which leg
// failed and what to pass instead — and collapsing it would print the
// answer's second half without its first.
func callFailure(port uint32, err error) error {
	if line, code, ok := notRunning(port, callDeadline, err); ok {
		return &exitStatus{code: code, err: errors.New(line)}
	}
	if cerr, ok := err.(*connect.Error); ok { //nolint:errorlint // an unwrapped one only; see above
		return &exitStatus{code: 1, err: fmt.Errorf("%s: %s", cerr.Code(), unprefixed(cerr.Message()))}
	}
	return &exitStatus{code: 1, err: err}
}

// unprefixed is one of voya's own diagnostics with [voyaDiagnostic] taken
// off the front, for the one place that puts it back: main prints every
// error as "voya: %v", so a body that already begins with the prefix — every
// routing diagnostic internal/proxy writes does — reaches the terminal
// saying voya twice, once around the other. Only the front of it: the
// diagnostic's later lines are its own. A message that never carried the
// prefix, which is a service's own refusal, comes back unchanged.
func unprefixed(text string) string { return strings.TrimPrefix(text, voyaDiagnostic) }

// reflectionHint is the sentence that follows a schema request the callee did
// not implement: unimplemented is exactly the answer a service serving no
// reflection gives, and -descriptors is what #158 decided that developer
// should reach for. Only for that code — a connection that failed or a
// deadline that expired has nothing to do with descriptors, and an
// instruction to compile some would send the reader the wrong way.
func reflectionHint(err error) string {
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		return ""
	}
	return "\n\nThat service serves no grpc.reflection.v1 (ADR 004 asks a fleet service to). " +
		"Pass its compiled descriptors instead: voya call -descriptors FILE …"
}

// dynamic is one message whose type this process learned at run time: the
// request pxf parsed against the callee's descriptor, or the reply it will
// parse the callee's bytes into. connect's generics take any pair of types
// and leave what they mean to the codec, so this is the whole of what a
// *dynamicpb.Message needs to travel through them — nothing here is a
// descriptor voya holds (ADR 003), and none of it outlives the command.
type dynamic struct {
	msg *dynamicpb.Message
}

// dynamicCodec is the binary protobuf codec of ADR 003, for messages with no
// Go type: it marshals whatever [dynamic] carries and unmarshals into a
// fresh message of the descriptor it was built with — the callee's response
// type on the client side.
//
// Its name is "proto", which is what puts `application/proto` on the wire
// and makes this the Connect protocol's binary codec rather than a fourth
// content type nothing else speaks.
type dynamicCodec struct {
	into protoreflect.MessageDescriptor
}

func (c *dynamicCodec) Name() string { return "proto" }

func (c *dynamicCodec) Marshal(msg any) ([]byte, error) {
	d, ok := msg.(*dynamic)
	if !ok {
		return nil, fmt.Errorf("the binary codec was handed a %T", msg)
	}
	return proto.Marshal(d.msg)
}

func (c *dynamicCodec) Unmarshal(data []byte, msg any) error {
	d, ok := msg.(*dynamic)
	if !ok {
		return fmt.Errorf("the binary codec was handed a %T", msg)
	}
	d.msg = dynamicpb.NewMessage(c.into)
	return proto.Unmarshal(data, d.msg)
}

func callUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  voya call [-descriptors FILE] SERVICE PACKAGE.SERVICE/METHOD [REQUEST]

Call a registered service by name, through voya, with the request written as
pxf and the reply printed as pxf: binary protobuf on the wire, the way the
fleet's own clients call each other (ADR 003). SERVICE is the registered name
— "payments", not a URL — and the call goes to the proxy under
payments.internal, so a reply is also proof that voya routes the name.

The request is the third word, or stdin when there is none or it is "-" —
at a terminal that means typing it and ending with ^D. An empty request is
not a missing one: '' is the empty message, which is what a procedure taking
no arguments is called with.

The schema comes from the callee's own grpc.reflection.v1, fetched for this
one call and forgotten; -descriptors reads a compiled FileDescriptorSet
instead, for a service that serves no reflection.

  -descriptors FILE   the schema and its whole import closure, as written by
                      "buf build -o" or "protoc --descriptor_set_out
                      --include_imports"; skips reflection

Unary procedures only: a streaming one is refused by name, exit 2. The reply
document is stdout and everything else is stderr, so the output can be piped.
Exit 0 on a reply, 1 on an error — the Connect code and message, or voya's
routing diagnostic when the proxy answered — and 2 for a refusal or a voya
that accepted and did not answer.

Examples:
  voya call payments payments.v1.Payments/GetBalance 'account = "acct_1"'
  echo 'account = "acct_1"' | voya call payments payments.v1.Payments/GetBalance
  voya call payments payments.v1.Payments/Ping ''
`)
}
