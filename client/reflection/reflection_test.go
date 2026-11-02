package reflection_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/grpcreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/client/reflection"
	registryv1connect "github.com/trendvidia/voya/proto/voya/registry/v1/registryv1connect"
)

// served is the name every case here reflects on: voya's own registration
// service, whose generated file declares the constant a caller is meant to
// pass. Using a real linked-in service rather than a fixture is the point —
// it is the same shape a fleet service is in, and it makes the resolution
// below a real lookup in the compiled file set.
const served = registryv1connect.RegistrationServiceName

// recordingMux records what was mounted where without routing anything, so
// the mounting contract can be asserted apart from the serving one.
type recordingMux struct{ paths []string }

func (m *recordingMux) Handle(pattern string, _ http.Handler) {
	m.paths = append(m.paths, pattern)
}

// TestMountServesBothProtocolVersions pins the decision a service should not
// have to make: which version a reflection client speaks is the client's,
// and grpcurl still opens v1alpha, so a service that mounted only v1 would
// answer half its callers with an unimplemented that reads as "no schema".
func TestMountServesBothProtocolVersions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mount func(m *recordingMux) error
	}{
		{"Mount", func(m *recordingMux) error { return reflection.Mount(m, served) }},
		{"MountFrom", func(m *recordingMux) error {
			return reflection.MountFrom(m, protoregistry.GlobalFiles, served)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &recordingMux{}
			if err := tc.mount(m); err != nil {
				t.Fatalf("mount: %v", err)
			}
			if len(m.paths) != 2 {
				t.Fatalf("mounted %d handler(s) at %q, want both protocol versions", len(m.paths), m.paths)
			}
			var v1, v1alpha bool
			for _, p := range m.paths {
				switch {
				case strings.Contains(p, "v1alpha"):
					v1alpha = true
				case strings.Contains(p, "v1"):
					v1 = true
				}
			}
			if !v1 || !v1alpha {
				t.Errorf("mounted %q, want one v1 path and one v1alpha path", m.paths)
			}
		})
	}
}

// TestMountedReflectionResolvesTheService is the end of the obligation: a
// real reflection client asks for the symbol and gets the file that declares
// it, which is exactly what `voya call` does to build a request.
func TestMountedReflectionResolvesTheService(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mount func(mux *http.ServeMux) error
	}{
		{"Mount", func(mux *http.ServeMux) error { return reflection.Mount(mux, served) }},
		{"MountFrom", func(mux *http.ServeMux) error {
			return reflection.MountFrom(mux, protoregistry.GlobalFiles, served)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			if err := tc.mount(mux); err != nil {
				t.Fatalf("mount: %v", err)
			}
			// HTTP/2, because ServerReflectionInfo is a bidi stream and
			// HTTP/1.1 cannot carry one — a service serving reflection is
			// serving it over h2, which is what voya's proxy speaks
			// upstream anyway (ADR 003).
			srv := httptest.NewUnstartedServer(mux)
			srv.EnableHTTP2 = true
			srv.StartTLS()
			t.Cleanup(srv.Close)

			stream := grpcreflect.NewClient(srv.Client(), srv.URL).NewStream(context.Background())
			t.Cleanup(func() { _, _ = stream.Close() })
			fds, err := stream.FileContainingSymbol(served)
			if err != nil {
				t.Fatalf("FileContainingSymbol(%s): %v", served, err)
			}
			if len(fds) == 0 {
				t.Fatal("reflection answered with no file descriptors")
			}
			// The file that declares it, not merely an answer: an empty or
			// wrong file set would satisfy a length check alone.
			var found bool
			for _, fd := range fds {
				for _, svc := range fd.GetService() {
					if fd.GetPackage()+"."+svc.GetName() == served {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("reflection answered without the file declaring %s", served)
			}
		})
	}
}

// TestMountFromUsesTheResolverItWasGiven is the control the case above
// cannot be: it passes the *global* registry, so a MountFrom that quietly
// ignored its resolver and read the compiled-in file set would pass it
// unchanged. Here the resolver is empty and the symbol is one the global
// registry certainly has, so an answer proves the resolver was bypassed.
func TestMountFromUsesTheResolverItWasGiven(t *testing.T) {
	mux := http.NewServeMux()
	if err := reflection.MountFrom(mux, &protoregistry.Files{}, served); err != nil {
		t.Fatalf("MountFrom: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	stream := grpcreflect.NewClient(srv.Client(), srv.URL).NewStream(context.Background())
	t.Cleanup(func() { _, _ = stream.Close() })
	if _, err := stream.FileContainingSymbol(served); err == nil {
		t.Fatalf("an empty resolver answered for %s, so MountFrom read the compiled-in "+
			"file set instead of the resolver it was handed", served)
	}
}

// TestReflectionNeedsHTTP2 is the test behind the sentence Mount's doc and
// the README both carry: mounting is only half of serving, and a service that
// hands this mux to a default http.Server answers the bidi stream with 505
// rather than with a schema — mounted, and useless, which is the failure the
// doc exists to keep a service author out of.
func TestReflectionNeedsHTTP2(t *testing.T) {
	mux := http.NewServeMux()
	if err := reflection.Mount(mux, served); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	srv := httptest.NewServer(mux) // HTTP/1.1: no EnableHTTP2, no TLS.
	t.Cleanup(srv.Close)

	stream := grpcreflect.NewClient(srv.Client(), srv.URL).NewStream(context.Background())
	t.Cleanup(func() { _, _ = stream.Close() })
	_, err := stream.FileContainingSymbol(served)
	if err == nil {
		t.Fatal("HTTP/1.1 carried a bidi reflection stream; the doc's HTTP/2 requirement is stale")
	}
	if !strings.Contains(err.Error(), "505") {
		t.Errorf("FileContainingSymbol over HTTP/1.1 = %v, want the 505 the doc names", err)
	}
}

// TestMountingNothingIsRefused is the no-op-safety criterion. An empty
// reflector mounts happily and then answers "not found" for every symbol the
// service really serves, so `voya call` reports a missing schema for an RPC
// that works — a wiring mistake that surfaces far from its cause. It is a
// startup answer instead.
func TestMountingNothingIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mount func(m *recordingMux) error
	}{
		{"Mount", func(m *recordingMux) error { return reflection.Mount(m) }},
		{"MountFrom", func(m *recordingMux) error {
			return reflection.MountFrom(m, protoregistry.GlobalFiles)
		}},
		{"MountFrom with no resolver", func(m *recordingMux) error {
			return reflection.MountFrom(m, nil, served)
		}},
		// A blank entry is the same wiring mistake one step later — an unset
		// environment variable read straight into the call — and it mounts a
		// reflector announcing a name no caller can ask by.
		{"Mount with a blank name", func(m *recordingMux) error { return reflection.Mount(m, served, " ") }},
		{"MountFrom with a blank name", func(m *recordingMux) error {
			return reflection.MountFrom(m, protoregistry.GlobalFiles, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &recordingMux{}
			err := tc.mount(m)
			if err == nil {
				t.Fatal("mounting was accepted; an empty reflector answers not-found for everything")
			}
			if len(m.paths) != 0 {
				t.Errorf("refused, but mounted %q anyway", m.paths)
			}
			// The message has to say what to pass, since the caller reading
			// it is the one who left the list out.
			if !strings.Contains(err.Error(), "voya/reflection") {
				t.Errorf("the error does not name this package: %v", err)
			}
		})
	}
	// The sentinel is comparable, so a caller can tell this apart from a
	// resolver problem without matching on text — for the blank entry too,
	// which wraps it rather than returning a second unrelated error.
	if err := reflection.Mount(&recordingMux{}); !errors.Is(err, reflection.ErrNoServices) {
		t.Errorf("Mount with no names = %v, want ErrNoServices", err)
	}
	if err := reflection.Mount(&recordingMux{}, ""); !errors.Is(err, reflection.ErrNoServices) {
		t.Errorf("Mount with a blank name = %v, want ErrNoServices", err)
	}
}

// TestMountServesNothingOnFly pins ADR 004's amendment of 2026-11-02: reflection
// is for the developer holding the terminal, and README's example mounts it
// unconditionally, so a service that follows the docs must not publish its
// schema in production. The control beside each case is what stops this passing
// because the mux never saw a call at all.
func TestMountServesNothingOnFly(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*recordingMux) error
	}{
		{"Mount", func(m *recordingMux) error { return reflection.Mount(m, served) }},
		{"MountFrom", func(m *recordingMux) error {
			return reflection.MountFrom(m, protoregistry.GlobalFiles, served)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(voya.FlyAppEnv, "")
			off := &recordingMux{}
			if err := tc.call(off); err != nil {
				t.Fatalf("off Fly: %v", err)
			}
			if len(off.paths) == 0 {
				t.Fatal("control: nothing mounted off Fly, so an empty mux below proves nothing")
			}

			t.Setenv(voya.FlyAppEnv, "payments")
			on := &recordingMux{}
			if err := tc.call(on); err != nil {
				t.Fatalf("on Fly: %v", err)
			}
			if len(on.paths) != 0 {
				t.Errorf("mounted %q with %s set; the schema would be public in production",
					on.paths, voya.FlyAppEnv)
			}
		})
	}
}

// TestMountStillRefusesAMistakeOnFly keeps a wiring mistake from depending on
// where the service runs: a service whose list is empty fails at startup on the
// developer's machine and on Fly alike.
func TestMountStillRefusesAMistakeOnFly(t *testing.T) {
	t.Setenv(voya.FlyAppEnv, "payments")
	if err := reflection.Mount(&recordingMux{}); !errors.Is(err, reflection.ErrNoServices) {
		t.Errorf("Mount with no names on Fly: err = %v, want ErrNoServices", err)
	}
	if err := reflection.MountFrom(&recordingMux{}, nil, served); err == nil {
		t.Error("MountFrom with a nil resolver on Fly: err = nil, want a refusal")
	}
}
