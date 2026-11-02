// Package requestlog is ADR 005's request log: one record per request the
// proxy answers, held in a bounded in-memory ring buffer (#150).
//
// It is metadata and nothing else — timestamp, authority, method, path,
// status, duration, upstream port, and the sequence number the log itself
// assigns each record. voya reads no payloads (ADR 003), so a record carries
// no body, no query string and no header beyond the two the line names, and
// the proxy is the only writer.
//
// Three things read it. The daemon prints each record as it lands, at
// LOG_LEVEL_INFO on `voya run`'s stderr; DiagService.ListRequests copies the
// buffer out for `voya log`; and DiagService.WatchRequests streams each record
// as it is recorded, for `voya log -f`. The one rendering all three share is
// [Request.String], so the live line and the stored form read the same.
package requestlog

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// Capacity is how many requests the daemon's buffer holds — the last 1024.
//
// It is a session's worth of a debug loop, not a history (ADR 005: "no
// history; a voya restart loses the request log"). The question the log
// answers is "what happened to *this* request", asked within seconds of
// making it; a browser frontend loading a page fires tens of requests
// through the proxy, and a developer iterating on one call fires one at a
// time, so a thousand records is many iterations of the busiest loop and an
// afternoon of the quietest. Past that, the request in question has scrolled
// off `voya run`'s terminal too. Each record is a few short strings and
// three numbers, so the buffer costs well under a megabyte full.
const Capacity = 1024

// Request is one line of the log: what the proxy knew about an exchange on
// its way out, and nothing it had to read a payload to learn.
type Request struct {
	// At is when the request reached the proxy handler.
	At time.Time
	// Authority is the :authority / Host it arrived with — the name voya
	// routed on, or failed to — less voya's own proxy port when the caller
	// named it, so that an h2 caller and an HTTP/1.1 one reading the same
	// service record it the same way and the column can be grepped for a
	// name (#161). A port that is not voya's is kept: see the proxy's
	// recordedAuthority, which is the only writer.
	Authority string
	Method    string
	// Path is the request path alone. The query string is deliberately not
	// here: a Connect GET carries its message in it, and that is payload.
	Path string
	// Status is the HTTP status written to the caller — the routing status
	// voya wrote, or the upstream's. A gRPC caller answered Trailers-Only
	// reads 200 here and its outcome in grpc-status, which is the protocol's
	// shape, not the log's.
	Status   int
	Duration time.Duration
	// UpstreamPort is where the request was forwarded, and 0 when voya
	// answered it without forwarding — every diagnostic row but one: a
	// registered upstream that refused the connection was forwarded to, and
	// its port is the developer's only handle on which process is gone, so
	// that row keeps it.
	UpstreamPort uint32
	// Sequence is the record's place in the log: [Log.Record] assigns it,
	// from 1, and a value the caller set is overwritten — so 0 on a record
	// read from the wire means a daemon that predates the field, not a first
	// record. It is what tells two records apart when the clock cannot
	// (#191): `voya log -f` dedupes the overlap between its two answers by it.
	// A record that has not been through a Log — the live line's copy under
	// [Tee] — carries none.
	Sequence uint64
}

// String is the human rendering, and the only one: the live line on `voya
// run`'s stderr and every line `voya log` prints are this. Timestamp in the
// local zone to the millisecond, then the authority, method and path,
// status, duration to the microsecond, and where it went — the upstream port,
// or "voya" when voya answered it itself.
func (r Request) String() string {
	to := "voya"
	if r.UpstreamPort != 0 {
		to = strconv.FormatUint(uint64(r.UpstreamPort), 10)
	}
	return fmt.Sprintf("%s %s %s %s %d %s -> %s",
		r.At.Local().Format("2006-01-02T15:04:05.000Z07:00"),
		printable(r.Authority), printable(r.Method), printable(r.Path), r.Status,
		r.Duration.Round(time.Microsecond), to)
}

// printable is s as a terminal may safely be handed it. Everything in a request
// is the caller's, and the caller can be any web page: an image tag can put an
// escape sequence or a newline in a path, and the line goes to a developer's
// terminal. A string with nothing to hide is returned as it is, so ordinary
// lines read as they always did; any other is quoted Go-style, which is what
// keeps a forged line from passing as a real one.
func printable(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !strconv.IsPrint(r) }) < 0 {
		return s
	}
	return strconv.Quote(s)
}

// Proto is the record on the wire: voya.diag.v1.Request, field for field.
func (r Request) Proto() *diagv1.Request {
	return &diagv1.Request{
		At:           timestamppb.New(r.At),
		Authority:    r.Authority,
		Method:       r.Method,
		Path:         r.Path,
		Status:       int32(min(max(r.Status, 0), 999)), //nolint:gosec // an HTTP status is three digits
		Duration:     durationpb.New(r.Duration),
		UpstreamPort: r.UpstreamPort,
		Sequence:     r.Sequence,
	}
}

// FromProto is [Request.Proto]'s inverse: how `voya log` gets from what
// DiagService sent to the same rendering the daemon printed.
func FromProto(m *diagv1.Request) Request {
	return Request{
		At:           m.GetAt().AsTime(),
		Authority:    m.GetAuthority(),
		Method:       m.GetMethod(),
		Path:         m.GetPath(),
		Status:       int(m.GetStatus()),
		Duration:     m.GetDuration().AsDuration(),
		UpstreamPort: m.GetUpstreamPort(),
		Sequence:     m.GetSequence(),
	}
}

// Recorder is what the proxy writes to: one call per request, on the way
// out. [*Log] is one; the daemon's tee of the buffer and the live line is
// another.
type Recorder interface {
	Record(Request)
}

// RecorderFunc adapts a function to [Recorder].
type RecorderFunc func(Request)

// Record calls f.
func (f RecorderFunc) Record(r Request) { f(r) }

// Tee records to every recorder in turn, so the daemon can hand the proxy
// one Recorder that both fills the buffer and prints the live line.
func Tee(recorders ...Recorder) Recorder {
	return RecorderFunc(func(r Request) {
		for _, rec := range recorders {
			rec.Record(r)
		}
	})
}

// Log is the bounded ring buffer, plus the followers reading it live.
//
// A fixed-size slice and a cursor: [Log.Record] overwrites the oldest entry
// once the slice is full, and [Log.Snapshot] reads them back oldest first.
// Safe for concurrent use — Record runs inside every proxy handler at once.
type Log struct {
	mu       sync.Mutex
	ring     []Request
	next     int // the slot Record writes next
	full     bool
	seq      uint64 // the last Sequence assigned; 0 before the first Record
	watchers map[*Watcher]struct{}
}

// New returns an empty log holding at most capacity requests. capacity below
// one is treated as one: a log that holds nothing is a Log nobody meant.
func New(capacity int) *Log {
	return &Log{ring: make([]Request, max(capacity, 1)), watchers: make(map[*Watcher]struct{})}
}

// Record appends r, dropping the oldest record if the buffer is full, and
// hands a copy to every watcher. It never blocks on a watcher: one that has
// fallen a whole buffer behind is ended instead — see [Watcher].
//
// The record's [Request.Sequence] is assigned here, under the lock, so it is
// the order the buffer accepted records in — the one thing about a record
// that two calls cannot share, which is why `voya log -f` keys on it. The
// buffer and every watcher see the same number for the same record.
func (l *Log) Record(r Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	r.Sequence = l.seq
	l.ring[l.next] = r
	l.next++
	if l.next == len(l.ring) {
		l.next = 0
		l.full = true
	}
	for w := range l.watchers {
		select {
		case w.ch <- r:
		default:
			// The proxy handler is what is calling, and it does not wait
			// for a follower that stopped reading. The follower's channel
			// closes, and [Watcher.Lagged] is what tells it that this was
			// the close rather than its own — internal/diag's
			// WatchRequests reads it to choose the code it ends with.
			w.lagged = true
			close(w.ch)
			delete(l.watchers, w)
		}
	}
}

// Len is how many records the buffer holds right now.
func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.len()
}

func (l *Log) len() int {
	if l.full {
		return len(l.ring)
	}
	return l.next
}

// Snapshot copies the buffer out, oldest first. limit above zero keeps only
// the newest limit records; zero means everything the buffer holds.
func (l *Log) Snapshot(limit int) []Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.len()
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Request, 0, n)
	// The oldest of the n we want sits n slots behind the write cursor.
	start := (l.next - n + len(l.ring)) % len(l.ring)
	for i := range n {
		out = append(out, l.ring[(start+i)%len(l.ring)])
	}
	return out
}

// Watcher is one follower: everything recorded from [Log.Watch] on, until
// [Watcher.Close] — never the buffer's history, which is [Log.Snapshot]'s.
type Watcher struct {
	log    *Log
	ch     chan Request
	lagged bool
	once   sync.Once
}

// Watch registers a follower. Its channel holds a buffer's worth of records
// the follower has not read yet; a follower that lets it fill is ended rather
// than waited on, because the writer is the proxy handler. Close it when done.
func (l *Log) Watch() *Watcher {
	w := &Watcher{log: l, ch: make(chan Request, len(l.ring))}
	l.mu.Lock()
	l.watchers[w] = struct{}{}
	l.mu.Unlock()
	return w
}

// C delivers each record as it lands. It is closed when the watcher is
// closed, or when it lagged — [Watcher.Lagged] tells the two apart, and is
// only meaningful once C is closed.
func (w *Watcher) C() <-chan Request { return w.ch }

// Lagged reports whether the log ended this watcher for falling a whole
// buffer behind. Read it after C closes; before that it is false by
// definition.
func (w *Watcher) Lagged() bool {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	return w.lagged
}

// Close unregisters the watcher and closes its channel. Idempotent, and safe
// after the log ended the watcher itself.
func (w *Watcher) Close() {
	w.once.Do(func() {
		w.log.mu.Lock()
		defer w.log.mu.Unlock()
		if _, live := w.log.watchers[w]; live {
			delete(w.log.watchers, w)
			close(w.ch)
		}
	})
}
