// Package demo is the bit of these examples that is NOT what a real service
// writes, kept in one place so the rest of them can be.
//
// A fleet service has a .proto file and generated bindings, and gets all
// three of the things below from them: the procedure string, the service name
// a reflection client asks by, and the schema the reflection server answers
// with. It writes `voyareflect.Mount(mux, paymentsv1connect.PaymentsServiceName)`
// and is done — README's "Using voya from a service" shows that one-liner,
// and it is what you should copy.
//
// These examples build the same three things by hand, from descriptors,
// because a .proto of their own would need a second buf module: proto-lint,
// proto-breaking and proto-check are all scoped to proto/, so example
// bindings would either drift with nothing checking them or those gates would
// have to grow, which is release-path surface an example does not justify
// (#293). test/acceptance's echoSchema is the same trade for the same reason.
//
// Nothing else in examples/ is unusual. If you are reading these to learn how
// to write a service, read payments/main.go and billing/main.go and treat
// this file as the generated code you would have.
package demo

import (
	"path"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Procedure is the one procedure these examples serve, in the form connect-go
// wants: a leading slash, the fully-qualified service, then the method.
//
// `voya call` takes it without the leading slash —
// `voya call payments.internal:9777 voya.example.v1.Payments/Charge …`.
const Procedure = "/voya.example.v1.Payments/Charge"

// Service is the name a reflection client asks by, derived from [Procedure]
// rather than written again: a reflection server that names a service its
// handler does not serve answers questions about a procedure nobody can call.
var Service = strings.TrimPrefix(path.Dir(Procedure), "/")

// Method is [Procedure]'s method half.
var Method = path.Base(Procedure)

// Schema is what payments serves reflection from — the descriptors of the
// file [Procedure] would have been compiled from.
//
// google/protobuf/wrappers.proto is in the set because the request and reply
// are a StringValue: a reflection answer that omitted it would be a file the
// caller cannot resolve, and `voya call` would fail building the request
// rather than at the call.
//
// It panics rather than returning an error because everything it reads is a
// constant in this file: a failure is this file being wrong, which is every
// caller's problem and not one caller's to handle.
func Schema() *protoregistry.Files {
	wrappers := protodesc.ToFileDescriptorProto(wrapperspb.File_google_protobuf_wrappers_proto)
	const stringValue = ".google.protobuf.StringValue"
	svc := protoreflect.FullName(Service)

	file := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("voya/example/v1/payments.proto"),
		Package:    proto.String(string(svc.Parent())),
		Syntax:     proto.String("proto3"),
		Dependency: []string{wrappers.GetName()},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String(string(svc.Name())),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String(Method),
				InputType:  proto.String(stringValue),
				OutputType: proto.String(stringValue),
			}},
		}},
	}

	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{wrappers, file},
	})
	if err != nil {
		panic("examples: building the demo schema: " + err.Error())
	}
	return files
}
