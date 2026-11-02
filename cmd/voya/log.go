package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http/httptrace"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"google.golang.org/protobuf/proto"

	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/requestlog"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// logCmd is `voya log` (#159): the request log ADR 005 keeps in the daemon,
// read from any terminal on the machine through DiagService under
// voya.internal on the proxy port. The snapshot is ListRequests, oldest
// first, one line per request in the rendering the daemon's own live line
// uses — the two forms read the same; -n N keeps the last N. -f prints the
// snapshot and then follows WatchRequests, one line per new request, until
// interrupted: ^C exits 0, and the daemon going away ends the follow with one
// line saying so and exit 1. -pxf renders each request as one pxf document
// on one line, so a pipe reads them as they come. -no-voya leaves out the
// rows recorded under the reserved voya.internal, snapshot and follow alike
// — [underReserved] carries why the daemon records them and the reader
// filters.
//
// The log is the command's product and goes to stdout; what the command says
// about itself — the follow ending, the log being off, the empty buffer —
// goes to stderr, so `voya log -pxf | …` carries pxf and nothing else.
//
// Two kinds of line read the other way in the human form, and deliberately: a
// voya that is not there, and a voya that accepts and never answers, are the
// two things the snapshot can *answer* rather than fail to render — #151 put
// them on stdout with the verdict in the exit code, where the sentence is the
// product. Under -pxf they follow the rest onto stderr, because stdout is
// then a document or empty and nothing else (#163); [prose] is the one place
// that choice is made and [unary] over [diagTarget.notRunning] the one place
// all three commands get the lines from, and
// TestDiagCommandsWhenVoyaIsNotRunning pins the stream and the code for each
// in both flag states. Past the snapshot the same two diagnoses mean
// something else — a daemon that went away mid-follow, after answering once —
// and that is a failure of the follow, on stderr with the rest.
func logCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("voya log", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	last := fs.Uint64("n", 0, "print only the last N requests")
	follow := fs.Bool("f", false, "follow: print the snapshot, then each request as it happens")
	asPXF := fs.Bool("pxf", false, "render each request as pxf, one document per line")
	noVoya := fs.Bool("no-voya", false, "leave out requests answered under the reserved voya.internal")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			logUsage(stdout)
			return nil
		}
		logUsage(stderr)
		return err
	}
	if fs.NArg() != 0 {
		logUsage(stderr)
		return fmt.Errorf("log takes no arguments, got %q", strings.Join(fs.Args(), " "))
	}
	if *last > math.MaxUint32 {
		return fmt.Errorf("-n %d is more requests than the log could ever hold", *last)
	}

	ctx := context.Background()
	target, err := dialDiag(ctx)
	if err != nil {
		return err
	}
	defer target.close()

	render := renderHuman
	if *asPXF {
		render = renderPXF
	}
	note := prose(*asPXF, stdout, stderr)

	// The follow is opened *before* the snapshot is asked for — see [follower]
	// for the window that closes — and nothing it carries is printed until the
	// snapshot has been.
	var f *follower
	if *follow {
		f = startFollow(ctx, target)
		defer f.stop()
	}

	// The snapshot.
	res, err := unary(ctx, target, note, target.client.ListRequests, &diagv1.ListRequestsRequest{Limit: uint32(*last)})
	if err != nil {
		return err
	}
	requests := res.GetRequests()
	if len(requests) == 0 && !*follow {
		// An answer, not a failure — but prose, so under -pxf it stays off
		// the stream a pipe is reading.
		fmt.Fprintln(note, "no requests yet")
		return nil
	}
	// Every record printed from the snapshot is one the stream may carry too:
	// it was recorded under an open watcher if it landed after [startFollow].
	// The keys are what [follower.print] drops the second copy by — less the
	// ones the stream has already delivered, which [follower.merge] folds
	// into the snapshot here so that what prints is in order (#189).
	var printed map[requestKey]struct{}
	if *follow {
		requests, printed = f.merge(requests)
	}
	for _, r := range requests {
		if *noVoya && underReserved(r.GetAuthority()) {
			continue
		}
		line, err := render(r)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, line)
	}
	if !*follow {
		return nil
	}
	return f.print(ctx, stdout, stderr, target, render, printed, *noVoya)
}

// follower is the WatchRequests stream `-f` holds open, read by a goroutine of
// its own from before the snapshot is asked for (#161).
//
// The order is the whole point. The snapshot is history and the stream is
// everything after it, so one of the two has to go first — and with the
// snapshot first, a request recorded in the loopback round trip between the
// answer and the watcher's registration is in neither, and the follower never
// prints it. connect-go gives the client no signal for "the watcher is
// registered" to wait on instead: its call blocks until the first message
// arrives, which for an idle log is never (internal/diag says why). Opened
// first, that request is in *both* — the stream carries it, and the snapshot
// taken while the stream is open still holds it — and two copies are a thing
// the client can fix, which one lost copy is not. [follower.print] drops the
// second by [requestKey].
//
// Opening it first is not the same as opening it earlier, though, and the
// order only holds because [startFollow] waits: a goroutine that has been
// spawned has not necessarily been scheduled, so without the wait the two
// calls race for the connection and the snapshot's round trip can finish
// before the watch's has started — the window the order exists to close,
// reopened at its full width. [startFollow] returns once the watch is on the
// wire (httptrace's WroteRequest), so what is left of the window is the
// moment between the call arriving at the daemon and the handler registering
// — the two streams travel one connection, in that order, and the daemon
// dispatches a handler per stream — which no round trip widens.
//
// The records that arrive before the snapshot has printed cannot be printed
// yet, so they queue: one buffer's worth, which is what the daemon's own
// watcher holds before it drops a follower that stopped reading
// ([requestlog.Log.Watch]) — past that the daemon ends the follow and says so,
// rather than this queue growing without bound.
type follower struct {
	requests chan *diagv1.Request
	err      chan error    // the stream's end, once
	sent     chan struct{} // closed once the call is on the wire, or cannot be
	cancel   context.CancelFunc
	queued   func() // [followQueued] as it stood when this follower was built
}

// startFollow opens the stream and starts reading it, and returns once the
// call has reached the wire — which is what puts it ahead of the snapshot
// ([follower] says why that matters). The caller stops it with
// [follower.stop].
func startFollow(ctx context.Context, target *diagTarget) *follower {
	ctx, cancel := context.WithCancel(ctx)
	f := &follower{
		requests: make(chan *diagv1.Request, requestlog.Capacity),
		err:      make(chan error, 1),
		sent:     make(chan struct{}),
		cancel:   cancel,
		queued:   followQueued,
	}
	// Once: an h2 request written a second time after a GOAWAY traces twice,
	// and the deferred call below is a third way here.
	sent := sync.OnceFunc(func() { close(f.sent) })
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { sent() },
	})
	go func() {
		defer close(f.requests)
		// The call ended without ever being written — nothing accepted, or
		// the dial failed. Waiting for a wire it never reached would hang the
		// command; the snapshot is about to make the same diagnosis anyway.
		defer sent()
		f.err <- f.receive(ctx, target)
	}()
	// Bounded by the deadline one unary call gets, for the one end that is
	// neither: a listener that accepts and then says nothing traces no write
	// and returns no error. Falling through leaves the diagnosis to the
	// snapshot's own deadline — `voya … did not answer within 2s`, exit 2 —
	// where hanging here would print nothing at all.
	wait := time.NewTimer(diagDeadline)
	defer wait.Stop()
	select {
	case <-f.sent:
	case <-wait.C:
	}
	return f
}

// followQueued runs after each record the stream carries has been queued.
// It is a seam for one test: [TestVoyaLogFollowMergesTheWindowByTime] holds
// the snapshot until the record the stream carried has crossed into the
// queue, which no daemon can see from its side of the wire. It does nothing
// in production, and it is a package variable because [startFollow] builds
// the follower where no test can reach it.
//
// It is read once, on the caller's goroutine, and copied into the follower it
// builds — never from [follower.receive], which runs on a goroutine no caller
// joins: `logCmd` returns as soon as the printing does, and stop() only
// *starts* that goroutine's unwind. Read from there, a follow left over from
// an earlier test would race the next test's assignment and its restore, and
// would count that test's records into a counter it does not own.
var followQueued = func() {}

// receive holds the stream open, queueing each record, and returns what ended
// it. The error channel is buffered, so this never waits for a reader that has
// gone.
func (f *follower) receive(ctx context.Context, target *diagTarget) error {
	stream, err := target.client.WatchRequests(ctx, connect.NewRequest(&diagv1.WatchRequestsRequest{}))
	if err != nil {
		return err
	}
	for stream.Receive() {
		select {
		case f.requests <- stream.Msg().GetRequest():
			f.queued()
		case <-ctx.Done():
			// Stopped with records still queued: nobody is reading them.
			return ctx.Err()
		}
	}
	return stream.Err()
}

// stop ends the stream and the goroutine reading it.
func (f *follower) stop() { f.cancel() }

// merge folds what the stream has carried so far into the snapshot, ordered
// by time, and returns that plus the keys of the snapshot records the stream
// has *not* yet delivered — the set [follower.print] drops second copies by.
//
// It exists because `-n` truncates the snapshot while the stream carries
// everything from the moment it opened, so the two answers legitimately
// overlap and only one of them is ordered (#189): a request recorded inside
// the window and cut from the snapshot by `-n` is older than everything the
// snapshot printed, and printing it from the stream afterwards puts an older
// line under a newer one — which reads as a bug in the daemon rather than as
// a startup artefact. Dropping it instead would be the silent loss #161
// removed, on a narrower window; so the two are merged, once, here.
//
// The cost is bounded to this call: what has queued by the time the snapshot
// is in hand is taken without waiting, and nothing after it is held or
// reordered — the live path is [follower.print], unchanged. A stream record
// still in flight at that instant — the daemon's watch handler is a goroutine
// hop behind the log, and so is the reader here — prints after the snapshot,
// as before; the merge narrows the window, it does not close it, and nothing
// short of holding the follow open for a settling time could.
//
// The dedupe is by [keyOf], as in print: a snapshot record the stream has
// delivered is one record, and its key comes off the returned set so a later
// request that repeated the key would still be printed.
func (f *follower) merge(snapshot []*diagv1.Request) ([]*diagv1.Request, map[requestKey]struct{}) {
	printed := make(map[requestKey]struct{}, len(snapshot))
	for _, r := range snapshot {
		printed[keyOf(r)] = struct{}{}
	}
	var queued []*diagv1.Request
drain:
	for {
		select {
		case r, ok := <-f.requests:
			if !ok {
				// The stream has ended; print's own receive sees the same
				// close and reports it.
				break drain
			}
			key := keyOf(r)
			if _, dup := printed[key]; dup {
				delete(printed, key)
				continue
			}
			queued = append(queued, r)
		default:
			break drain
		}
	}
	if len(queued) == 0 {
		return snapshot, printed
	}
	merged := make([]*diagv1.Request, 0, len(snapshot)+len(queued))
	for len(snapshot) > 0 && len(queued) > 0 {
		if recordedBefore(queued[0], snapshot[0]) {
			merged, queued = append(merged, queued[0]), queued[1:]
		} else {
			merged, snapshot = append(merged, snapshot[0]), snapshot[1:]
		}
	}
	merged = append(merged, snapshot...)
	merged = append(merged, queued...)
	return merged, printed
}

// recordedBefore orders two records the way the daemon recorded them: by At,
// and by sequence when the clock did not move between them (#191). Equal on
// both — a daemon that predates the sequence, inside one clock tick — the
// snapshot's record goes first, since it is the older answer.
func recordedBefore(a, b *diagv1.Request) bool {
	if as, bs := a.GetAt().GetSeconds(), b.GetAt().GetSeconds(); as != bs {
		return as < bs
	}
	if an, bn := a.GetAt().GetNanos(), b.GetAt().GetNanos(); an != bn {
		return an < bn
	}
	return a.GetSequence() < b.GetSequence()
}

// print writes each record the stream carries, one line per request, until the
// stream ends or ^C — skipping the ones printed from the snapshot already.
//
// ^C ends the follow, and only the follow, which is why the handler is
// installed here rather than where the stream was opened.
// signal.NotifyContext takes the process's default disposition away for as
// long as it is installed, so installing it before the snapshot would turn a
// ^C during that bounded call into a cancelled ListRequests reported as `voya:
// canceled`, exit 1, instead of the ordinary interrupt.
// TestVoyaLogFollowLeavesTheSnapshotsInterruptAlone holds it here. Ahead of
// this line a signal is the shell's; from here it ends the stream and `voya
// log` exits 0.
func (f *follower) print(ctx context.Context, stdout, stderr io.Writer, target *diagTarget,
	render func(*diagv1.Request) (string, error), printed map[requestKey]struct{}, noVoya bool,
) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			// Our own ^C. Exit 0, quietly; the deferred stop ends the stream.
			return nil
		case r, ok := <-f.requests:
			if !ok {
				return f.ended(ctx, target, stderr)
			}
			key := keyOf(r)
			if _, dup := printed[key]; dup {
				// Recorded between the stream opening and the snapshot being
				// taken, and printed from the snapshot already. Dropped from
				// the set as well as from the output, so a later request that
				// repeated the key would still be printed.
				delete(printed, key)
				continue
			}
			if noVoya && underReserved(r.GetAuthority()) {
				continue
			}
			line, err := render(r)
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, line)
		}
	}
}

// ended is what a follow whose stream stopped says. ctx is [follower.print]'s,
// so a cancellation of ours is told from the daemon's end by asking it first:
// the two race whenever a ^C arrives as the stream is ending, and our own
// cancellation reads from here exactly like the daemon's end — the record
// channel closing.
//
// The question is a non-blocking receive on Done rather than `ctx.Err() !=
// nil`, which nilerr reads as an error found and dropped on the floor.
//
// The stream's error is read after that question, and reading it never waits:
// [startFollow]'s goroutine sends it before the deferred close of the record
// channel, and that close is what brought us here.
func (f *follower) ended(ctx context.Context, target *diagTarget, stderr io.Writer) error {
	select {
	case <-ctx.Done():
		// Our own ^C: the stream ended because we left. Exit 0, quietly.
		return nil
	default:
	}
	err := <-f.err
	if err == nil {
		// A stream the daemon ended by returning nothing: connect reports a
		// handler's nil as a clean end, and `%v` of it would print the `<nil>`
		// that the flag [requestlog.Watcher.Lagged] exists to keep out of this
		// line. Only [diag.Server.WatchRequests]'s unreachable branch returns
		// nil today, so this is a guard on that invariant and not a path
		// through it — the exit code is the same 1, because a follow that
		// stopped while the daemon lives is still not a follow.
		fmt.Fprintln(stderr, "voya: the follow ended")
		return &exitStatus{code: 1}
	}
	if line, code, ok := target.notRunning(err); ok {
		fmt.Fprintln(stderr, line)
		return &exitStatus{code: code}
	}
	// The daemon ended the stream — on its way down (unavailable, with its
	// own words), by dropping a follower that fell behind, or by the
	// connection going away under a voya that was killed outright. One line
	// saying so, exit 1: the one thing a follower must never do is print
	// nothing and exit 0 while the daemon it was following stops.
	fmt.Fprintf(stderr, "voya: the follow ended: %v\n", connectMessage(err))
	return &exitStatus{code: 1}
}

// requestKey identifies one request across the two answers `-f` reads, and is
// what the overlap between them is deduped by: the record's sequence number,
// which [requestlog.Log.Record] assigns as the buffer accepts the record, and
// nothing else (#191). The key used to be the timestamp, the authority and the
// path (#161), and that is a guess about identity: two polls of the same path
// inside one clock tick — 15.6 ms by default on Windows — read as one request,
// and the second was dropped as the first's duplicate. The sequence is the
// log's own count, exact on every platform.
//
// A daemon that predates the field sends 0 for every record, and keying on
// that alone would read every stream record as a copy of the first snapshot
// one; those keys fall back to the old triple, so a `voya log` newer than the
// daemon it follows loses nothing it did not lose before. The daemon starts
// its count at 1 so the two cases can be told apart.
type requestKey struct {
	sequence  uint64
	seconds   int64
	nanos     int32
	authority string
	path      string
}

func keyOf(r *diagv1.Request) requestKey {
	if seq := r.GetSequence(); seq != 0 {
		return requestKey{sequence: seq}
	}
	return requestKey{
		seconds:   r.GetAt().GetSeconds(),
		nanos:     r.GetAt().GetNanos(),
		authority: r.GetAuthority(),
		path:      r.GetPath(),
	}
}

// underReserved reports whether a recorded authority is the reserved
// voya.internal — the question the router asked of the same header when it
// sent the request to voya's own handler, asked again here through the same
// [registry.ServiceFromAuthority]. Single-sourced deliberately, and not by a
// comparison of its own: the recorded authority is the header as it arrived
// less voya's own proxy port, so it still carries whatever the client wrote
// — a foreign port, a root dot, uppercase — and a second normalisation rule
// here is how a row comes to be answered under the name and kept by the
// filter, or refused under it and hidden. `voya.internal:abc` is the case
// that shows both edges: the router does not route it, so its row is the
// developer's own diagnostic and stays.
//
// It is what `-no-voya` filters by, applied to the snapshot and the follow
// alike, and it lives on this side of the wire by decision (#218): the
// daemon records every request it answers under the name — a validator's key-set
// fetch, a validator reading /.well-known/jwks.json — because a log whose
// completeness depended on what callers claim about themselves would not be
// a log of what happened. Suppression destroys information and filtering
// does not, so leaving voya's own rows out is the reader's choice, made
// here. The one skip the daemon keeps is its own DiagService answers, for
// the loop the log-skip in internal/proxy names.
func underReserved(authority string) bool {
	name, ok := registry.ServiceFromAuthority(authority)
	return ok && name == registry.Reserved
}

// connectMessage is a connect error's own message, without the code prefix
// connect.Error.Error() adds: `voya: the request log is off: …` reads as
// voya's sentence, `voya: failed_precondition: the request log is off` as a
// stack trace. A non-connect error — a connection reset under a follower —
// is returned as is.
func connectMessage(err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return errors.New(cerr.Message())
	}
	return err
}

// renderHuman is the daemon's own live line, so `voya log` and `voya run`'s
// terminal read the same (#150).
func renderHuman(r *diagv1.Request) (string, error) {
	return requestlog.FromProto(r).String(), nil
}

// renderPXF is one request as one pxf document on one line: what
// pxf.Marshal writes, with its line breaks folded to spaces. That is still
// the same document — pxf entries are whitespace-separated, and Marshal
// quotes and escapes every string, so no value spans a line — and one line
// per document is what lets `voya log -f -pxf | …` hand a reader whole
// documents as they come, and `grep` keep working on the stream (ADR 003
// puts request logging under the pxf rule).
func renderPXF(r *diagv1.Request) (string, error) { return pxfLine(r) }

// pxfLine is [renderPXF] for any message whose place is a stream. `status`
// and `routes` answer one document, not a stream, and write pxf.Marshal's
// form as it comes.
func pxfLine(m proto.Message) (string, error) {
	b, err := pxf.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("rendering pxf: %w", err)
	}
	return strings.ReplaceAll(strings.TrimRight(string(b), "\n"), "\n", " "), nil
}

func logUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  voya log [-n N] [-f] [-pxf] [-no-voya]

Print the request log the daemon keeps: one line per request the proxy
answered, oldest first — timestamp, authority, method, path, status,
duration, and the upstream port (or "voya" when voya answered it itself).

  -n N      only the last N requests
  -f        follow: print the snapshot, then each request as it happens; ^C stops
  -pxf      render each request as pxf, one document per line
  -no-voya  leave out requests answered under voya's own voya.internal, such
            as a validator's key-set fetch; -n counts before this filter
`)
}
