package acceptance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// --- #7: what a restart under load is allowed to do to a caller --------------

// outcomes is what a caller saw across a restart, sorted into the three things
// that can happen to a request.
type outcomes struct {
	mu sync.Mutex

	// served counts answers per instance, which is what says traffic was
	// actually flowing on both sides of the restart rather than the load having
	// quietly stopped.
	served map[string]int

	// explained counts requests that failed while nothing held the name, with
	// voya's diagnostic saying so. A restart has a window where the service is
	// genuinely not running, and a request arriving in it has to fail — ADR 005
	// only requires that it be told why.
	explained int

	// cutOff collects the failures this test exists to forbid: a request that
	// died on the wire rather than being answered. Kept in full, because one is
	// a failure and the message is the evidence.
	cutOff []string
}

func newOutcomes() *outcomes { return &outcomes{served: map[string]int{}} }

// record classifies one call.
//
// The distinction is not the status code. A Connect caller gets `unavailable`
// for every routing failure voya reports — the code is fixed by the protocol —
// so what separates "voya answered" from "the request was cut off" is which
// diagnostic came back. "no running instance" is voya declining to route,
// which is correct in the gap. "the connection failed" is voya having routed
// to a listener that was already gone, which is the race the shutdown grace
// exists to close; so is any error that is not voya's at all, since that is the
// transport reporting a broken connection.
func (o *outcomes) record(identity string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	switch {
	case err == nil:
		o.served[identity]++
	case strings.Contains(err.Error(), "has no running instance"):
		o.explained++
	default:
		o.cutOff = append(o.cutOff, err.Error())
	}
}

func (o *outcomes) snapshot() (served map[string]int, explained int, cutOff []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	served = make(map[string]int, len(o.served))
	for k, n := range o.served {
		served[k] = n
	}
	return served, o.explained, append([]string(nil), o.cutOff...)
}

// loadUntilStopped keeps n callers calling in a loop until the returned
// function is called, spread round-robin over names — or over other.internal
// if none are given.
//
// o may be nil, which is background traffic: something for voya to be busy
// with while a different quantity is measured, with no claim made about how
// any of it turned out. Non-nil is the load this test asserts on.
func loadUntilStopped(t *testing.T, v *voyad, n int, o *outcomes, names ...string) func() {
	t.Helper()
	if len(names) == 0 {
		names = []string{"other"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	for i := range n {
		client := callerTo(t, v, names[i%len(names)])
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				got, err := callCtx(ctx, client)
				if ctx.Err() != nil {
					// Cancelled mid-call as the test wound down. Not an outcome
					// voya produced, and counting it would be this test failing
					// itself on the way out.
					return
				}
				if o != nil {
					o.record(identity(got), err)
				}
			}
		}()
	}
	return func() { cancel(); wg.Wait() }
}

// callCtx is call with a context, so the load can be stopped mid-flight.
func callCtx(ctx context.Context, c *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue]) (string, error) {
	resp, err := c.CallUnary(ctx, connect.NewRequest(wrapperspb.String("ping")))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetValue(), nil
}

// identity is the instance half of what the echo handler answers with.
func identity(answer string) string {
	name, _, _ := strings.Cut(answer, "|")
	return name
}

// TestARestartUnderLoadCutsNothingOff is #7's acceptance criterion: a service
// restarts while callers are talking to it, and not one request dies on the
// wire.
//
// Some requests do fail, and must — for part of a restart there is genuinely no
// instance to route to. What separates a correct restart from the one this
// issue was filed about is that every one of those failures is voya explaining
// itself, and none is a connection reset arriving at a caller that has no idea
// why. The reset is what a developer reads as flakiness in their own service,
// and the shutdown grace is what removes it: the callee stays routable until
// voya has stopped routing to it, so its listener never closes underneath a
// request voya has already sent.
//
// It does detect the thing it forbids. Run against a client built with
// WithShutdownGrace(0), it failed on all three attempts, 2 to 4 requests dying
// on the wire each time. That is why the assertion is on zero rather than on a
// rate.
//
// Read the totals it logs for what they are. Most of the requests in a run are
// refusals carrying voya's diagnostic, not answers: the callee is down for a
// fraction of a second per restart — see [service.shutdown] for where that
// goes — and eight callers spinning against a proxy that answers "no running
// instance" immediately pile up thousands of them. The refusals are not the
// evidence and neither is the total; the evidence is the zero, and the
// perGeneration floor below is what says answers were flowing on both sides of
// every restart.
//
// The count is a reading of the machine, not of voya, so do not treat any
// figure here as a threshold. What it does track is the callee's downtime, and
// #52 cut that: on one host, back to back, a run went from 374 answered against
// 71,833 refused to 416 against 21,796, and the test itself from 4.5s to 1.3s.
// An earlier host published ~260 against ~25,700 for the same unfixed code,
// which is the measure of how much the absolute numbers are about the hardware.
func TestARestartUnderLoadCutsNothingOff(t *testing.T) {
	// Several restarts rather than one. The window a cut-off can happen in is
	// the few milliseconds around the callee leaving, and the rest of a restart
	// is the new process starting up — so one restart puts almost all of its
	// requests where nothing can go wrong. Repeating is what gives the
	// assertion its power.
	const (
		generations   = 4
		perGeneration = 50 // answered calls required of each, before it is replaced
	)

	v := startVoya(t)
	identityOf := func(gen int) string { return "other/gen" + strconv.Itoa(gen) }

	// The first instance is up before any load starts. A caller that beats the
	// very first registration gets "no service named other" — a correct answer
	// to a question about cold start, and noise in a test about restarts.
	callee := startService(t, v, "other", identityOf(0))

	o := newOutcomes()
	stopLoad := loadUntilStopped(t, v, 8, o)
	defer stopLoad()

	for gen := range generations {
		want := identityOf(gen)
		waitFor(t, fmt.Sprintf("the load to reach %s", want), func() bool {
			served, _, _ := o.snapshot()
			return served[want] >= perGeneration
		})
		if gen == generations-1 {
			break
		}

		previous := callee.port()
		callee.shutdown()
		callee = startService(t, v, "other", identityOf(gen+1))
		if callee.port() == previous {
			t.Fatalf("the restarted callee reused port %d; the test would prove nothing", previous)
		}
	}
	stopLoad()

	served, explained, cutOff := o.snapshot()
	answered := 0
	for _, n := range served {
		answered += n
	}
	for _, err := range cutOff {
		t.Errorf("a request was cut off rather than answered or explained:\n%s", err)
	}
	if len(cutOff) > 0 {
		t.Fatalf("%d of %d requests died on the wire across %d restarts",
			len(cutOff), answered+explained+len(cutOff), generations-1)
	}
	// No second pass over the generations here. The loop above already blocks
	// on served[gen] >= perGeneration for every generation including the last,
	// and t.Fatalf's on timeout, so a re-check afterwards is an assertion that
	// cannot fail — which reads like coverage of a missed restart and is not.
	t.Logf("across %d restarts: %d answered, %d refused with voya's diagnostic while nothing "+
		"held the name, 0 cut off", generations-1, answered, explained)
}

// TestTheGraceOutlastsTheRouteChange is the same claim one level down, and
// without the load: after stop returns, voya is no longer routing to the
// service that just left. That is the property the grace is chosen to
// guarantee, and it is what makes it safe for a service to close its listener
// the moment stop hands control back.
func TestTheGraceOutlastsTheRouteChange(t *testing.T) {
	v := startVoya(t)

	for range 20 {
		s := startService(t, v, "other", "other/only")
		s.stop() // the grace is inside this
		if up, live := v.Registry().Lookup("other"); live {
			t.Fatalf("voya is still routing other.internal to port %d after stop returned; "+
				"a service closing its listener now would drop what voya sends next", up.Port)
		}
		s.shutdown()
	}
}
