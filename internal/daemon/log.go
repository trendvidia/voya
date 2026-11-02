package daemon

import (
	"bytes"
	"log"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// sink is the daemon's log: one function that prints, and the floor from the
// resolved configuration below which nothing reaches it.
//
// This is where `log_level` starts meaning something (#43). The field has
// been validated since #13 and read by nothing since, which is the worse
// shape of the two — a developer who writes `log_level = LOG_LEVEL_ERROR`
// gets a clean resolve, a clean validation, and exactly the output they had
// before, and the validation is what makes it look wired up.
//
// The levels live here rather than in the leaf packages on purpose. internal/
// proxy and internal/dns each take plain func(string, ...any)s and write one
// kind of message through each, so the daemon can hand them functions that
// are already either the real one or a no-op, and neither package has to
// learn what a level is. That held when the proxy grew a second kind of
// message: ADR 005's request log (#150) is a line at [info] per request
// beside the failures at [errorLevel], and the proxy takes them as two
// seams — [proxy.WithErrorLog] and the recorder [proxy.WithRequestLog] is
// handed — each given its own already-decided function here. So the sink
// still needs no per-message level, and the leaf packages still know none.
type sink struct {
	floor configv1.LogLevel
	print func(string, ...any)
}

// The severities this package writes at, named short because the call sites
// read better for it and the full enum spelling is three times the length of
// the message it qualifies.
const (
	errorLevel = configv1.LogLevel_LOG_LEVEL_ERROR
	warn       = configv1.LogLevel_LOG_LEVEL_WARN
	info       = configv1.LogLevel_LOG_LEVEL_INFO
)

// discard is what a message below the floor is printed by.
//
// A func rather than a var holding one: [sink.at] hands this same value to the
// proxy, the DNS server and both registry handlers at construction, so
// anything that reassigned it would be captured by objects outliving whatever
// did the reassigning.
func discard(string, ...any) {}

// newSink builds the daemon's log from the resolved level.
//
// floor is trusted to be a real level: [Start] runs config.Validate before it
// gets here, and validation rejects both an unknown number and
// LOG_LEVEL_UNSPECIFIED — the latter deliberately, so that a .pxf layer which
// clears the field fails loudly instead of silently going quiet
// (TestValidateRejects in internal/config covers both).
func newSink(floor configv1.LogLevel, print func(string, ...any)) *sink {
	if print == nil {
		print = discard
	}
	return &sink{floor: floor, print: print}
}

// at returns the function messages of severity level are printed by: the real
// one when the level passes the floor, and a no-op when it does not.
//
// Handing back a function rather than taking the message is what lets a
// listener be given its severity once, at construction, instead of every
// package below here growing a level argument.
func (s *sink) at(level configv1.LogLevel) func(string, ...any) {
	if !s.allows(level) {
		return discard
	}
	return s.print
}

// allows reports whether a message of severity level survives the floor.
//
// The enum numbers severity descending — ERROR is 1 and DEBUG is 4 — so a
// message passes when its number is at most the configured one. That is the
// "verbosity floor" the config schema documents, read the only way the
// numbering allows: LOG_LEVEL_ERROR prints failures and nothing else, and
// each level above it adds the next band.
func (s *sink) allows(level configv1.LogLevel) bool {
	return level <= s.floor
}

// stdLogger bridges logf onto the *log.Logger the standard library insists on
// for its own messages.
//
// net/http writes what the server layer notices — a handler that panicked, an
// h2 preface that never arrived, a superfluous WriteHeader — through
// http.Server.ErrorLog, and falls back to the standard logger's stderr when
// that field is nil ($GOROOT/src/net/http/server.go, (*Server).logf). Leaving
// it nil would put those lines outside the sink entirely: printed at every
// level, including the two a developer sets to be quiet, and on a stream
// nothing else here writes to. internal/proxy makes the same argument one
// layer down, about httputil's messages, in [proxy.WithErrorLog].
func stdLogger(logf func(string, ...any)) *log.Logger {
	return log.New(writerFunc(func(line []byte) {
		// log.Logger appends the newline it is about to be handed back; the
		// sink's callers write one line per call and add their own.
		logf("%s", bytes.TrimRight(line, "\n"))
	}), "", 0)
}

// writerFunc adapts a log function to the io.Writer a [log.Logger] writes
// through.
type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}
