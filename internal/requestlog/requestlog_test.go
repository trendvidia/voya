package requestlog

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"google.golang.org/protobuf/proto"
)

// req is the i-th request of a test: the path carries i so order is
// checkable, and the rest are fixed values a mistaken field mapping would
// misplace.
func req(i int) Request {
	return Request{
		At:           time.Date(2026, 8, 15, 17, 46, 15, 123456789, time.UTC).Add(time.Duration(i) * time.Second),
		Authority:    "payments.internal",
		Method:       "POST",
		Path:         "/n/" + strconv.Itoa(i),
		Status:       200 + i,
		Duration:     time.Duration(i+1) * 1234567 * time.Nanosecond,
		UpstreamPort: uint32(50000 + i),
	}
}

func paths(rs []Request) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Path)
	}
	return out
}

func TestSnapshotIsOldestFirstAndBounded(t *testing.T) {
	const n = 4
	l := New(n)
	if got := l.Snapshot(0); len(got) != 0 {
		t.Fatalf("an empty log snapshots %v", got)
	}

	// Under capacity: everything, in order.
	for i := range 3 {
		l.Record(req(i))
	}
	if got, want := paths(l.Snapshot(0)), []string{"/n/0", "/n/1", "/n/2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("under capacity: %v, want %v", got, want)
	}
	if l.Len() != 3 {
		t.Errorf("Len = %d, want 3", l.Len())
	}

	// N+k through a buffer of N leaves exactly the last N, in order.
	for i := 3; i < n+3; i++ {
		l.Record(req(i))
	}
	if got, want := paths(l.Snapshot(0)), []string{"/n/3", "/n/4", "/n/5", "/n/6"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after wrapping: %v, want the last %d, %v", got, n, want)
	}
	if l.Len() != n {
		t.Errorf("Len = %d, want the capacity %d", l.Len(), n)
	}

	// The whole record survives, not only the path — plus the sequence the
	// log gave it: the seventh record is number 7.
	last := l.Snapshot(0)[n-1]
	want := req(6)
	want.Sequence = 7
	if last != want {
		t.Errorf("last record = %+v, want %+v", last, want)
	}
}

// TestRecordAssignsTheSequence: the log numbers records as it accepts them,
// from 1, and the number is the log's — a value the caller set is replaced,
// and the buffer and a watcher see the same one for the same record (#191).
// It does not restart when the ring wraps: it counts records, not slots.
func TestRecordAssignsTheSequence(t *testing.T) {
	l := New(8)
	w := l.Watch()
	defer w.Close()
	for i := range 5 {
		r := req(i)
		r.Sequence = 99 // the caller's guess, which the log overrules
		l.Record(r)
	}
	buffered := l.Snapshot(0)
	if len(buffered) != 5 {
		t.Fatalf("the buffer holds %d records, want the 5 recorded — the numbering below would pass vacuously", len(buffered))
	}
	for i, r := range buffered {
		if r.Sequence != uint64(i+1) {
			t.Errorf("record %d in the buffer carries sequence %d, want %d", i, r.Sequence, i+1)
		}
	}
	for want := uint64(1); want <= 5; want++ {
		select {
		case r := <-w.C():
			if r.Sequence != want {
				t.Errorf("the watcher saw sequence %d, want %d", r.Sequence, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no record delivered for sequence %d", want)
		}
	}
	// A fresh log starts over at 1 — it is the daemon's count, not a global
	// — and a ring that wraps keeps counting: two slots, five records, the
	// buffer holds 4 and 5.
	l2 := New(2)
	for i := range 5 {
		l2.Record(req(i))
	}
	got := l2.Snapshot(0)
	// Read the sequences out before judging them: naming got[0] and got[1]
	// inside the failure message would panic on the short snapshot the guard
	// exists to report.
	seqs := make([]uint64, 0, len(got))
	for _, r := range got {
		seqs = append(seqs, r.Sequence)
	}
	if len(seqs) != 2 || seqs[0] != 4 || seqs[1] != 5 {
		t.Errorf("after five records in a two-slot ring, the buffer holds sequences %v, want [4 5]", seqs)
	}
	l3 := New(2)
	l3.Record(req(0))
	if got := l3.Snapshot(0)[0].Sequence; got != 1 {
		t.Errorf("a new log's first record is sequence %d, want 1 — 0 is reserved for a daemon that predates the field", got)
	}
}

func TestSnapshotLimitKeepsTheNewest(t *testing.T) {
	l := New(4)
	for i := range 6 { // wraps once, so the limit is tested across the seam
		l.Record(req(i))
	}
	for _, tc := range []struct {
		limit int
		want  string
	}{
		{0, "/n/2,/n/3,/n/4,/n/5"},
		{1, "/n/5"},
		{2, "/n/4,/n/5"},
		{4, "/n/2,/n/3,/n/4,/n/5"},
		{9, "/n/2,/n/3,/n/4,/n/5"}, // more than held: everything held
	} {
		if got := strings.Join(paths(l.Snapshot(tc.limit)), ","); got != tc.want {
			t.Errorf("Snapshot(%d) = %s, want %s", tc.limit, got, tc.want)
		}
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	l := New(2)
	l.Record(req(0))
	snap := l.Snapshot(0)
	l.Record(req(1))
	l.Record(req(2)) // overwrites slot 0
	want := req(0)
	want.Sequence = 1
	if snap[0] != want {
		t.Errorf("a snapshot changed under a later Record: %+v", snap[0])
	}
}

func TestNewClampsCapacityToOne(t *testing.T) {
	l := New(0)
	l.Record(req(0))
	l.Record(req(1))
	if got := paths(l.Snapshot(0)); len(got) != 1 || got[0] != "/n/1" {
		t.Errorf("a zero-capacity log holds %v, want the one newest record", got)
	}
}

func TestWatchDeliversOnlyWhatFollows(t *testing.T) {
	l := New(8)
	l.Record(req(0)) // history: never replayed
	w := l.Watch()
	defer w.Close()
	l.Record(req(1))
	l.Record(req(2))

	for _, want := range []string{"/n/1", "/n/2"} {
		select {
		case r := <-w.C():
			if r.Path != want {
				t.Errorf("watched %s, want %s", r.Path, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no record delivered for %s", want)
		}
	}
	select {
	case r, ok := <-w.C():
		t.Fatalf("watcher delivered %v (open=%v) with nothing recorded since; the history replayed?", r, ok)
	default:
	}
}

func TestCloseEndsAWatcherAndStopsDeliveries(t *testing.T) {
	l := New(8)
	w := l.Watch()
	w.Close()
	w.Close() // idempotent
	if _, ok := <-w.C(); ok {
		t.Fatal("a closed watcher's channel is still open")
	}
	if w.Lagged() {
		t.Error("a watcher closed by its owner reads as lagged")
	}
	l.Record(req(0)) // must not panic on a closed channel
	if n := len(l.watchers); n != 0 {
		t.Errorf("%d watchers still registered after Close", n)
	}
}

// A watcher that stops reading is ended, not waited on: the writer is the
// proxy handler, and its capacity is the buffer's.
func TestALaggingWatcherIsEndedNotWaitedOn(t *testing.T) {
	const capacity = 4
	l := New(capacity)
	w := l.Watch()
	defer w.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range capacity + 1 { // one more than the channel holds
			l.Record(req(i))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a watcher that was not reading")
	}

	// The records that fit are still delivered, then the channel closes,
	// and Lagged says why.
	delivered := 0
	for range w.C() {
		delivered++
	}
	if delivered != capacity {
		t.Errorf("delivered %d records before the end, want the channel's %d", delivered, capacity)
	}
	if !w.Lagged() {
		t.Error("the watcher was ended by the log and does not say it lagged")
	}
	if n := len(l.watchers); n != 0 {
		t.Errorf("%d watchers still registered after one lagged", n)
	}
	// And a fresh watcher is unaffected by the old one's fate.
	fresh := l.Watch()
	defer fresh.Close()
	l.Record(req(9))
	select {
	case r := <-fresh.C():
		if r.Path != "/n/9" {
			t.Errorf("fresh watcher got %s", r.Path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a fresh watcher after a lagged one received nothing")
	}
}

func TestStringIsTheOneRendering(t *testing.T) {
	r := Request{
		At:           time.Date(2026, 8, 15, 17, 46, 15, 123456789, time.UTC),
		Authority:    "payments.internal",
		Method:       "POST",
		Path:         "/voya.acceptance.v1.Echo/Whoami",
		Status:       200,
		Duration:     1234567 * time.Nanosecond,
		UpstreamPort: 54321,
	}
	// The zone is the machine's; pin the rest by rendering the same instant
	// the same way.
	wantAt := r.At.Local().Format("2006-01-02T15:04:05.000Z07:00")
	if got, want := r.String(), wantAt+" payments.internal POST /voya.acceptance.v1.Echo/Whoami 200 1.235ms -> 54321"; got != want {
		t.Errorf("String() = %q\n            want %q", got, want)
	}
	if !strings.HasPrefix(wantAt, "2026-08-1") || !strings.Contains(wantAt, ".123") {
		t.Errorf("the timestamp is not rendered to the millisecond in the local zone: %q", wantAt)
	}

	// voya answered it itself: the port column says so.
	r.UpstreamPort = 0
	r.Status = 404
	if got := r.String(); !strings.HasSuffix(got, " 404 1.235ms -> voya") {
		t.Errorf("a request voya answered renders as %q, want it to end in `-> voya`", got)
	}
}

func TestProtoRoundTrips(t *testing.T) {
	r := req(3)
	r.Sequence = 42
	back := FromProto(r.Proto())
	// Timestamp precision is nanoseconds either way; the location is not
	// carried, so compare instants.
	if !back.At.Equal(r.At) {
		t.Errorf("At: %v -> %v", r.At, back.At)
	}
	back.At, r.At = time.Time{}, time.Time{}
	if back != r {
		t.Errorf("round trip changed the record: %+v -> %+v", r, back)
	}
	// And the same wire message twice, so the conversion is deterministic.
	if !proto.Equal(req(3).Proto(), req(3).Proto()) {
		t.Error("Proto() differs between two calls on equal records")
	}
}

func TestTeeRecordsToEveryRecorder(t *testing.T) {
	var a, b []Request
	tee := Tee(RecorderFunc(func(r Request) { a = append(a, r) }), RecorderFunc(func(r Request) { b = append(b, r) }))
	tee.Record(req(0))
	tee.Record(req(1))
	if len(a) != 2 || len(b) != 2 || a[1] != req(1) || b[0] != req(0) {
		t.Errorf("tee delivered a=%v b=%v", paths(a), paths(b))
	}
}

// TestStringEscapesWhatACallerControls is the line a hostile page cannot write
// to a terminal: control bytes in any field a caller sets come out as text, and
// the record stays one line. An ordinary record is the control, unchanged.
func TestStringEscapesWhatACallerControls(t *testing.T) {
	at := time.Unix(0, 0)
	hostile := Request{
		At:        at,
		Authority: "pay\x1b]0;x\x07ments.internal",
		Method:    "GE\x1bT",
		Path:      "/a\x1b[2J\n2026-01-01T00:00:00.000Z forged.internal GET / 200 1µs -> 8080\xc2\x9b",
	}
	line := hostile.String()
	if strings.IndexFunc(line, func(r rune) bool { return r != ' ' && !unicode.IsGraphic(r) }) >= 0 {
		t.Errorf("line carries a control character: %q", line)
	}
	if strings.Contains(line, "\x1b") || strings.Contains(line, "\n") {
		t.Errorf("line carries a raw escape or newline: %q", line)
	}

	plain := Request{At: at, Authority: "payments.internal", Method: "GET", Path: "/a/é b", Status: 200}
	if want := "payments.internal GET /a/é b 200"; !strings.Contains(plain.String(), want) {
		t.Errorf("ordinary line changed: %q does not contain %q", plain.String(), want)
	}
}
