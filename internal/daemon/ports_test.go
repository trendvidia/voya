package daemon

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/lsof"
)

// TestTheListenerTableNamesTheRightTransports is the fact the duplicated port
// helpers forgot, and the one [FreePorts] now derives everything else from.
//
// It asks the sockets [Start] actually bound rather than restating the table
// here. A second copy of the three networks would catch an edit to the first
// copy and nothing else; the property that has to hold is that the table
// agrees with what voya binds, because if it drifts, every port draw in both
// suites goes back to being a coin flip — silently, since a TCP reservation
// for a UDP port succeeds and tells you nothing.
func TestTheListenerTableNamesTheRightTransports(t *testing.T) {
	d, err := Start(context.Background(), testConfig(t, time.Second), func(string, ...any) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	for _, tc := range []struct {
		l     listener
		addrs []net.Addr
	}{
		{proxyListener, []net.Addr{d.proxyLn.Addr()}},
		{registrationListener, []net.Addr{d.registerLn.Addr()}},
		{dnsListener, d.dnsLn.Addrs()},
	} {
		// The table spells the network as lsof's -i selector wants it, and
		// net.Addr spells it as the net package does, so the comparison is
		// on the transport rather than on the casing. Same count and every
		// declared network bound: a listener on two transports (DNS, #184)
		// that quietly bound one would otherwise pass on the one it kept.
		if len(tc.addrs) != len(tc.l.networks) {
			t.Errorf("the %s listener declares %d network(s) %v, but it binds %d: %v",
				tc.l.name, len(tc.l.networks), tc.l.networks, len(tc.addrs), tc.addrs)
		}
		for _, network := range tc.l.networks {
			if !slices.ContainsFunc(tc.addrs, func(a net.Addr) bool {
				return strings.EqualFold(string(network), a.Network())
			}) {
				t.Errorf("the %s listener declares %q, but it binds only %v",
					tc.l.name, network, tc.addrs)
			}
		}
	}
}

// TestFreePortsAreDistinctAndBindable covers what a caller depends on: three
// numbers config.Validate will accept, each actually free on the transport
// that listener will bind it with.
//
// Binding each one back is the part that matters. A port drawn on the wrong
// transport still looks like a perfectly good number.
func TestFreePortsAreDistinctAndBindable(t *testing.T) {
	ports, err := FreePorts(t.Context())
	if err != nil {
		t.Fatalf("FreePorts: %v", err)
	}

	if ports.Proxy == ports.DNS || ports.Proxy == ports.Registration || ports.DNS == ports.Registration {
		t.Errorf("config.Validate compares the numbers, so these are a rejected configuration: "+
			"proxy=%d dns=%d registration=%d", ports.Proxy, ports.DNS, ports.Registration)
	}

	// The DNS port on both transports: dns.Listen binds UDP and TCP on the
	// one number (#184), so a draw free on UDP alone is the coin flip
	// [FreePorts] exists to stop.
	for _, tc := range []struct {
		name string
		port uint32
		udp  bool
		tcp  bool
	}{
		{"proxy", ports.Proxy, false, true},
		{"registration", ports.Registration, false, true},
		{"dns", ports.DNS, true, true},
	} {
		if tc.udp {
			conn, err := net.ListenPacket("udp", config.Addr(tc.port))
			if err != nil {
				t.Errorf("the %s port %d is not free on UDP, which is a transport it was drawn for: %v",
					tc.name, tc.port, err)
			} else {
				conn.Close()
			}
		}
		if tc.tcp {
			ln, err := net.Listen("tcp", config.Addr(tc.port))
			if err != nil {
				t.Errorf("the %s port %d is not free on TCP, which is a transport it was drawn for: %v",
					tc.name, tc.port, err)
			} else {
				ln.Close()
			}
		}
	}
}

// TestReserveRefusesAnUnknownTransport keeps the switch in [reserve] from
// falling through silently.
//
// A listener added to the table with a network nobody handled would otherwise
// return port 0, and the daemon would bind an ephemeral port instead of the
// one the test believes it chose — which is the kind of failure that reads as
// a flake somewhere else entirely.
func TestReserveRefusesAnUnknownTransport(t *testing.T) {
	_, sock, err := reserve(t.Context(), listener{name: "invented", networks: []lsof.Network{"SCTP"}})
	release(sock)
	if err == nil {
		t.Fatal("a listener with an unhandled network drew a port instead of failing")
	}
	if !strings.Contains(err.Error(), "invented") || !strings.Contains(err.Error(), "SCTP") {
		t.Errorf("the error names neither the listener nor its network: %v", err)
	}
}
