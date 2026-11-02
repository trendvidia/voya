package acceptance

// --- #154: grpc.health.v1 through the proxy -----------------------------------
//
// ADR 004 has every service serve grpc.health.v1 over Connect, so that "a
// plain HTTP POST to /grpc.health.v1.Health/Check works" — as a Fly HTTP
// check there, and here as what `voya health` probes: through the proxy,
// under the service's own authority, so a green cell also proves voya routes
// the name. This is the fixture-side proof; the command is tested where the
// binary is, in cmd/voya.

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

	voya "github.com/trendvidia/voya/client"
)

// healthClient is a grpc.health.v1 client aimed at name through v's proxy,
// dialled the way every caller in this suite dials: the client package's
// dialer, which sends every *.internal name to the proxy port.
func healthClient(t *testing.T, v *voyad, name string) *grpchealth.Client {
	t.Helper()
	transport := &http.Transport{
		Protocols:   httpProtocols(),
		DialContext: voya.Dialer(voya.WithProxyPort(v.proxyPort)),
	}
	t.Cleanup(transport.CloseIdleConnections)
	return grpchealth.NewClient(&http.Client{Transport: transport, Timeout: callerTimeout},
		"http://"+name+".internal:"+strconv.FormatUint(uint64(v.proxyPort), 10))
}

// TestHealthCheckReachesAServiceByName: a Check to other.internal through the
// proxy is answered by the registered service — SERVING, which is what the
// fixture says of itself — and a Check to a name nothing holds is voya's own
// diagnostic under `unavailable`, which is what `voya health` prints beside
// an unreachable entry: the answer names the service and the failure and
// arrives promptly rather than hanging.
func TestHealthCheckReachesAServiceByName(t *testing.T) {
	v := startVoya(t)
	startService(t, v, "other", "other/only")

	res, err := healthClient(t, v, "other").Check(context.Background(), &grpchealth.CheckRequest{})
	if err != nil {
		t.Fatalf("Check other.internal through the proxy: %v", err)
	}
	if res.Status != grpchealth.StatusServing {
		t.Errorf("other.internal reports %s, want serving", res.Status)
	}

	// The unregistered name: the proxy answers, and it is voya's diagnostic
	// that comes back — the detail `voya health` shows for a name it could
	// not reach.
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticDeadline)
	defer cancel()
	_, err = healthClient(t, v, "nobody").Check(ctx, &grpchealth.CheckRequest{})
	if err == nil {
		t.Fatal("Check nobody.internal answered, with nothing registered under the name")
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("Check nobody.internal failed with %v, want unavailable", err)
	}
	assertVoyaDiagnostic(t, err, "nobody")
}
