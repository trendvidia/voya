package daemon

import (
	"fmt"
	"testing"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// TestSinkPrintsEveryBandAtOrBelowTheFloor is the half of #43's checklist
// [TestLogLevelDecidesWhatIsPrinted] cannot cover.
//
// That test drives a real daemon, so it can only assert on messages a daemon
// can be made to emit — the startup banner and an eviction. Nothing there
// stages a route the proxy cannot serve or a DNS reply that will not send, so
// nothing asserts that an ERROR-severity message is ever printed at all, and
// a sink that returned [discard] for every severity would satisfy its
// absences just as happily. "Failures at LOG_LEVEL_ERROR" is the band the
// changelog leads with; this is where it is checked.
//
// The expectations are written out rather than computed. An expected value
// derived from `level <= floor` would be the implementation asserting about
// itself, and would hold for whatever comparison the code happened to make.
func TestSinkPrintsEveryBandAtOrBelowTheFloor(t *testing.T) {
	const debug = configv1.LogLevel_LOG_LEVEL_DEBUG

	for _, tc := range []struct {
		floor    configv1.LogLevel
		prints   []configv1.LogLevel
		silences []configv1.LogLevel
	}{
		{errorLevel, []configv1.LogLevel{errorLevel}, []configv1.LogLevel{warn, info}},
		{warn, []configv1.LogLevel{errorLevel, warn}, []configv1.LogLevel{info}},
		{info, []configv1.LogLevel{errorLevel, warn, info}, nil},
		{debug, []configv1.LogLevel{errorLevel, warn, info}, nil},
	} {
		t.Run(tc.floor.String(), func(t *testing.T) {
			for _, level := range tc.prints {
				if got, want := printedAt(tc.floor, level), "hello "+level.String(); got != want {
					t.Errorf("floor %s printed %q at %s, want %q", tc.floor, got, level, want)
				}
			}
			for _, level := range tc.silences {
				if got := printedAt(tc.floor, level); got != "" {
					t.Errorf("floor %s printed %q at %s, want nothing", tc.floor, got, level)
				}
			}
		})
	}
}

// printedAt is what a sink with the given floor writes for one message of the
// given severity, or "" when it writes nothing.
func printedAt(floor, level configv1.LogLevel) string {
	var got string
	s := newSink(floor, func(format string, args ...any) { got = fmt.Sprintf(format, args...) })
	s.at(level)("hello %s", level)
	return got
}

// TestStdLoggerReachesTheSink pins the bridge [Start] hands to both
// http.Servers. net/http will only write its own failures through a
// *log.Logger, and one built on anything but the sink puts them on stderr at
// every level — which is the shape #43 was filed about, for the one class of
// message a developer least wants to lose.
func TestStdLoggerReachesTheSink(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	// Trailing newline included: log.Logger adds one, and a sink whose
	// callers write [announcementFormat] themselves would otherwise gain a
	// blank line per http message.
	stdLogger(logf).Printf("http: panic serving %s", "127.0.0.1:1001")

	if len(lines) != 1 {
		t.Fatalf("stdLogger wrote %d lines, want 1: %q", len(lines), lines)
	}
	if got, want := lines[0], "http: panic serving 127.0.0.1:1001"; got != want {
		t.Errorf("stdLogger wrote %q, want %q", got, want)
	}
}

// TestStdLoggerAtAFloorThatSilencesIt is the other half: the bridge carries
// whatever function it was built from, so a floor that discards the band
// discards the http layer's messages with it rather than routing them around
// the sink.
func TestStdLoggerAtAFloorThatSilencesIt(t *testing.T) {
	var lines []string
	s := newSink(errorLevel, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	stdLogger(s.at(info)).Printf("something chatty")

	if len(lines) != 0 {
		t.Errorf("an INFO-band bridge printed %q at floor %s, want nothing", lines, errorLevel)
	}
}

// TestLogLevelsAreNumberedDescendingBySeverity pins what [sink.allows] reads
// off the enum.
//
// `level <= floor` is a severity comparison only because the numbers run
// ERROR=1 through DEBUG=4. A level added out of severity order — LOG_LEVEL_
// FATAL = 5 being the natural next number, and the one `buf breaking` is
// happiest with — would resolve and validate cleanly ([config.Validate]
// accepts any value the enum names) and then be suppressed at every floor a
// developer can currently write: voya silent on its loudest class of message,
// with nothing failing to say so. Renumbering is deliberate or it is a bug.
func TestLogLevelsAreNumberedDescendingBySeverity(t *testing.T) {
	descending := []configv1.LogLevel{
		configv1.LogLevel_LOG_LEVEL_ERROR,
		configv1.LogLevel_LOG_LEVEL_WARN,
		configv1.LogLevel_LOG_LEVEL_INFO,
		configv1.LogLevel_LOG_LEVEL_DEBUG,
	}
	for i, level := range descending {
		if got, want := int32(level), int32(i+1); got != want {
			t.Errorf("%s = %d, want %d — sink.allows reads severity off these numbers", level, got, want)
		}
	}
	// Every level the enum defines has to appear above, or a new one slips in
	// unplaced. +1 is LOG_LEVEL_UNSPECIFIED, which validation rejects and no
	// message is ever written at.
	if got, want := len(configv1.LogLevel_name), len(descending)+1; got != want {
		t.Errorf("LogLevel has %d values, want %d — a new level needs a severity position here and in sink.allows",
			got, want)
	}
}
