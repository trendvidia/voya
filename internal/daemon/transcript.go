package daemon

import (
	"fmt"
	"strings"
	"sync"
)

// Transcript is a logf that keeps what it was handed, so a test can assert on
// what voya printed.
//
// It lives here, in the package that emits the announcements, rather than in
// a helper beside each caller — the same argument [Ports] makes about the
// listener table. Both suites that read voya's output used to carry their own
// copy of this: a mutex, a slice of everything logf was handed, a prefix
// filter, and the same comment explaining the goroutines. A change to either
// copy — bounding retention, splitting multi-line announcements, a role-aware
// matcher — had to be found and repeated in the other, and the second copy is
// the one that stops being updated.
//
// The lock is not ceremony. This function is the daemon's, so the DNS serve
// loop and both http.Servers' ErrorLogs append from goroutines no test
// sequences, and an unguarded read would report a data race stacked on top of
// whatever assertion actually failed. Every method below takes it, including
// the one that only formats a failure message.
type Transcript struct {
	// tee is where messages also go as they arrive, or nil.
	//
	// Called with mu held, so the order a tee sees is the order the
	// transcript keeps. The goroutines below write without being sequenced by
	// anything, so releasing first would let two messages be stored in one
	// order and printed in the other — and reading in order is the whole of
	// what a tee is for.
	tee func(string, ...any)

	mu sync.Mutex
	// One entry per logf call, deliberately not one per line of text. Both
	// announcements ADR 002 obliges are several lines long — a heading, an
	// indented instance or two, and a closing sentence — so splitting on "\n"
	// would leave [Transcript.Announced] unable to tell where one
	// announcement ends and the next begins, which is the whole of what it
	// checks.
	messages []string
}

// NewTranscript returns a transcript to hand to [Start] as its logf.
//
// tee, if non-nil, is handed every message as it arrives, prefixed with
// "voya: ". Passing t.Logf is what lets a failure read in order alongside
// everything else the test logged, and a suite that does that has no need to
// render [Transcript.String] on failure.
//
// It is called with the transcript's lock held, for the ordering reason on
// the field, so it must not read the transcript back: [Transcript.Announced]
// and its neighbours take that same lock, and it is not reentrant.
func NewTranscript(tee func(format string, args ...any)) *Transcript {
	return &Transcript{tee: tee}
}

// Logf is the sink. It has the shape [Start] wants.
func (tr *Transcript) Logf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.messages = append(tr.messages, message)
	if tr.tee != nil {
		tr.tee("voya: %s", message)
	}
}

// Announced reports whether one of ADR 002's announcements contains want and
// all of more.
//
// Announcements only, by [AnnouncementMarker]: matching anywhere in the
// output would let the startup banner — which carries three ephemeral port
// numbers, and 51001 contains "1001" — answer for one of them.
//
// All of them inside the *same* announcement, because an eviction and the
// reclaim that follows it name the same instance, and a check that spanned
// two announcements would let either one answer for the other.
//
// One want is required rather than all of them being variadic, because a
// caller that built its wants from a slice and got an empty one would
// otherwise be told yes by every announcement voya has ever made — the answer
// no assertion here wants, arrived at without a diagnostic. The compiler is
// the place to catch that.
func (tr *Transcript) Announced(want string, more ...string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, message := range tr.messages {
		if strings.HasPrefix(message, AnnouncementMarker) && containsAll(message, want, more) {
			return true
		}
	}
	return false
}

// Printed reports whether voya wrote a message containing want and all of
// more anywhere at all, announcement or not. One want is required for the
// reason on [Transcript.Announced].
func (tr *Transcript) Printed(want string, more ...string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, message := range tr.messages {
		if containsAll(message, want, more) {
			return true
		}
	}
	return false
}

// String renders everything voya printed, for a failure message.
func (tr *Transcript) String() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return strings.Join(tr.messages, "\n")
}

func containsAll(s, want string, more []string) bool {
	if !strings.Contains(s, want) {
		return false
	}
	for _, w := range more {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}
