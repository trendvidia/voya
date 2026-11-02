package acceptance

// --- #158: grpc.reflection.v1 through the proxy --------------------------------
//
// ADR 004, amended 2026-08-16, puts a second obligation on a fleet service
// beside grpc.health.v1: serve reflection. `voya call <service>
// <Package.Service/Method>` has no descriptors of its own and voya holds none
// (ADR 003), so the schema comes from the callee itself, over the proxy,
// under the callee's own name — which makes a reflection request routing as
// much as it is schema. This is the fixture-side proof; the command is tested
// where the binary is, in cmd/voya.

import (
	"context"
	"net/http"
	"path"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/grpcreflect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	voya "github.com/trendvidia/voya/client"
)

// echoService is the service half of [procedure], which is the name a
// reflection client asks by. Derived rather than written a second time: a
// reflection server that names a service the handler does not serve answers
// questions about a procedure nobody can call.
var echoService = strings.TrimPrefix(path.Dir(procedure), "/")

// echoSchema is what this suite's services serve reflection from: the file
// [procedure] would have been compiled from, built here as descriptors
// because the fixture has no .proto of its own — proto/ is voya's own schema
// surface, and a service that exists to be called by tests does not belong in
// it.
//
// google/protobuf/wrappers.proto comes along because Whoami's request and
// reply are a StringValue, and a reflection answer that omitted it would be a
// file the caller cannot resolve. The registry below is what a reflection
// server walks, so what it can resolve is exactly what a caller receives.
func echoSchema() *protoregistry.Files {
	wrappers := protodesc.ToFileDescriptorProto(wrapperspb.File_google_protobuf_wrappers_proto)
	const stringValue = ".google.protobuf.StringValue"
	svc := protoreflect.FullName(echoService)
	echo := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("voya/acceptance/v1/echo.proto"),
		Package:    proto.String(string(svc.Parent())),
		Syntax:     proto.String("proto3"),
		Dependency: []string{wrappers.GetName()},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String(string(svc.Name())),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String(path.Base(procedure)),
				InputType:  proto.String(stringValue),
				OutputType: proto.String(stringValue),
			}},
		}},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{wrappers, echo},
	})
	if err != nil {
		// Not a t.Fatalf: this builds the fixture's own schema from constants
		// in this file, so a failure is this file being wrong for every test
		// that serves a handler, not one test's setup.
		panic("building the fixture schema: " + err.Error())
	}
	return files
}

// TestReflectionResolvesThroughVoya: a caller that knows only the service's
// name gets its schema from the service itself, through the proxy — the first
// half of what `voya call` does, and the half that has to be true of the
// fleet rather than of the command.
//
// The assertion is not that bytes came back but that they resolve:
// protodesc.NewFiles fails on a file whose dependencies are missing, so a
// server that answered with echo.proto alone would fail here rather than
// somewhere downstream in a caller that then cannot build the request.
func TestReflectionResolvesThroughVoya(t *testing.T) {
	v := startVoya(t)
	startService(t, v, "other", "other/only")

	// h2c only, not this suite's usual [httpProtocols]: reflection is a bidi
	// stream, and a cleartext dial has no ALPN to negotiate with, so offering
	// HTTP/1.1 as well is offering the one protocol a bidi stream cannot use —
	// the request hangs with its send side open rather than failing. `voya
	// call` dials the same way for the same reason (cmd/voya/diag.go).
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols:   protocols,
		DialContext: voya.Dialer(voya.WithProxyPort(v.proxyPort)),
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: callerTimeout}

	ctx, cancel := context.WithTimeout(context.Background(), diagnosticDeadline)
	defer cancel()
	stream := grpcreflect.NewClient(client,
		"http://other.internal:"+strconv.FormatUint(uint64(v.proxyPort), 10)).NewStream(ctx)
	defer func() { _, _ = stream.Close() }()
	fds, err := stream.FileContainingSymbol(protoreflect.FullName(echoService))
	if err != nil {
		t.Fatalf("asking other.internal for the schema of %s: %v", echoService, err)
	}

	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: fds})
	if err != nil {
		t.Fatalf("the schema other.internal served does not resolve: %v", err)
	}
	desc, err := files.FindDescriptorByName(protoreflect.FullName(echoService))
	if err != nil {
		t.Fatalf("the schema other.internal served has no %s: %v", echoService, err)
	}
	sd, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		t.Fatalf("%s came back as %T, want a service", echoService, desc)
	}
	md := sd.Methods().ByName(protoreflect.Name(path.Base(procedure)))
	if md == nil {
		t.Fatalf("%s has no %s", echoService, path.Base(procedure))
	}
	// The dependency closure, which is what makes the answer usable: a
	// caller builds its request against this descriptor.
	if got, want := md.Input().FullName(), protoreflect.FullName("google.protobuf.StringValue"); got != want {
		t.Errorf("%s takes %s, want %s", procedure, got, want)
	}
	if md.IsStreamingClient() || md.IsStreamingServer() {
		t.Errorf("%s came back as a streaming procedure", procedure)
	}
}
