// Package reflection serves `grpc.reflection.v1` for a service that has
// joined a voya fleet.
//
// ADR 004 puts two obligations on a fleet service. The first,
// `grpc.health.v1`, transfers to production unchanged and is one line in a
// service's template. The second arrived with `voya call` (#158): a caller
// with only a name has nowhere to get the callee's schema, so the callee
// serves reflection and voya asks it. This package is that obligation's one
// line, so the two are the same size at the call site.
//
// It is deliberately **not** part of [github.com/trendvidia/voya/client].
// That package's virtue is what it does not pull in — a service that only
// wants a name pays for a registration stream and nothing else — and
// `connectrpc.com/grpcreflect` is a dependency only the services that serve
// reflection should carry. Keeping it one import down means importing
// `client` still costs what it always did, which is the claim #36 exists to
// enforce when that package becomes a module of its own (#174) — at that
// split this directory needs a `go.mod` of its own, or `grpcreflect` lands
// in the client module's requirements and is inherited by services that
// import only [github.com/trendvidia/voya/client.Join], the same inheritance
// `client/voya.go`'s dependency note describes for chameleon.
//
// voya itself holds no descriptors and reads none of this (ADR 003): the
// file set served here is the service's own, resolved in the service's own
// process.
//
// On Fly it serves nothing. Reflection is for the developer holding the
// terminal (ADR 004), and a service mounts it unconditionally, so without this
// every service that followed the docs would publish its schema in production.
// [voya.OnFly] is the same test [voya.Join] uses. A service that wants reflection
// on Fly on purpose mounts grpcreflect's handlers itself.
package reflection

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/grpcreflect"
	"google.golang.org/protobuf/reflect/protodesc"

	voya "github.com/trendvidia/voya/client"
)

// ErrNoServices is what [Mount] and [MountFrom] return when they are given
// no usable service name — an empty list, or a name that is blank.
//
// Not a no-op, and not a mounted-but-empty reflector, because both fail the
// same way much later and much more confusingly: a reflection server that
// names nothing answers "not found" for every symbol the service actually
// serves, so `voya call` reports the schema missing while the RPC it is
// asking about works. A service that mounts reflection means to serve it,
// and an empty list is a wiring mistake worth failing at startup. A blank
// entry is that same mistake one step later — `Mount(mux, os.Getenv("SVC"))`
// with the variable unset — and it announces a name no caller can ask by, so
// it is refused on the same line.
var ErrNoServices = errors.New("voya/reflection: no usable service names; " +
	"pass the fully-qualified names this process serves (generated Connect files declare each as a constant)")

// Mux is the one method mounting needs. [http.ServeMux] satisfies it, as
// does every router that took its shape, so this package mounts handlers
// without deciding what a service routes with.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Mount serves reflection for the named services on mux, resolving their
// descriptors from the file set this binary was compiled with. When
// FLY_APP_NAME is set it mounts nothing and returns nil, though it still refuses
// the mistakes it refuses everywhere else.
//
// Both protocol versions are mounted — v1 and v1alpha — because which one a
// caller speaks is the caller's choice and not the service's: grpcurl and
// older clients still open v1alpha, and a service that mounted only v1 would
// answer them with an unimplemented that reads as "this service has no
// schema". Mounting both is what makes the obligation a decision the service
// does not have to make.
//
// The names are fully qualified — "voya.registry.v1.RegistrationService",
// not "RegistrationService" — and generated Connect files declare each one
// as a constant, which is what a caller should pass rather than a literal.
//
// The server this mux is handed to has to speak HTTP/2: ServerReflectionInfo
// is a bidirectional stream, and a default [net/http.Server] — HTTP/1.1, plus
// h2 only under TLS — answers one with "505 HTTP Version Not Supported", so
// the reflection mounted here reads to every client as no schema at all. On
// loopback that means cleartext h2, which is also the only thing voya's
// upstream leg speaks (ADR 003):
//
//	protocols := new(http.Protocols)
//	protocols.SetHTTP1(true)
//	protocols.SetUnencryptedHTTP2(true)
//	srv := &http.Server{Handler: mux, Protocols: protocols}
func Mount(mux Mux, services ...string) error {
	if err := checkNames(services); err != nil {
		return err
	}
	mount(mux, grpcreflect.NewStaticReflector(services...))
	return nil
}

// MountFrom is [Mount] for a process whose descriptors are not the ones it
// was compiled with — a fixture, a test harness, or anything holding a file
// set it built at run time. The resolver is where the file descriptors come
// from; everything else is Mount's, HTTP/2 included, and so is mounting nothing
// when FLY_APP_NAME is set.
//
// Extensions are the one thing it does not cover: grpcreflect's extension
// answers (`FileContainingExtension`, `AllExtensionNumbersOfType`) still come
// from [google.golang.org/protobuf/reflect/protoregistry.GlobalTypes], so a
// run-time schema that declares extensions serves them only if they are
// registered globally too.
func MountFrom(mux Mux, files protodesc.Resolver, services ...string) error {
	if err := checkNames(services); err != nil {
		return err
	}
	if files == nil {
		return errors.New("voya/reflection: nil resolver; use Mount for the compiled-in file set")
	}
	mount(mux, grpcreflect.NewReflector(
		grpcreflect.NamerFunc(func() []string { return services }),
		grpcreflect.WithDescriptorResolver(files),
	))
	return nil
}

// checkNames is the guard both constructors share. It reports the offending
// index, because the caller reading the error is the one holding the list.
func checkNames(services []string) error {
	if len(services) == 0 {
		return ErrNoServices
	}
	for i, name := range services {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%w: services[%d] is blank", ErrNoServices, i)
		}
	}
	return nil
}

// mount is the half both constructors share: the two handler versions, on
// the paths grpcreflect names for them. The one place that decides whether
// anything is served, so neither constructor can forget.
func mount(mux Mux, reflector *grpcreflect.Reflector) {
	if voya.OnFly() {
		return
	}
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
}
