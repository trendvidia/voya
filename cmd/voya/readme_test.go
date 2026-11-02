package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/requestlog"
)

// README's status note was, until #210, the one hand-written copy of which
// commands did not exist yet, held to [commands] by
// TestReadmeStatusNoteMatchesTheCommandsTable (#119). Building `token` —
// the last unbuilt command — emptied both sides of that comparison, and the
// test's own vacuity guard said to retire it with the "Not implemented yet"
// block, so the pin and its extraction machinery (statusNote,
// unbuiltCommandNames, TestStatusNoteExtraction) went with the block.
// TestEveryCommandIsBuilt in main_test.go is what remains: if a command is
// ever again added ahead of its implementation, the machinery — this file's
// included — has to come back with it.

// TestReadmeNamesTheRequestLogCapacity: README tells a reader how many
// requests the daemon's buffer holds, in prose, and [requestlog.Capacity] is
// the constant that decides it — with nothing tying the two together until
// this test (#163). The same drift #119 gave the status note a check for.
//
// The number only, never the wording, so prose edits do not fight it: any "the
// last N requests" in README has to be that constant. The match is asserted
// before it is read, because a re-wrap that split the phrase across a line
// break would otherwise leave this test passing over nothing.
//
// README alone. `changelog.d/added/request-log.md` and ADR 005's amendment
// name the number too, and deliberately keep it: a released note and a dated
// decision say what shipped and what was decided then, which a later capacity
// does not change. README is the one that has to be true today.
func TestReadmeNamesTheRequestLogCapacity(t *testing.T) {
	found := lastNRequests.FindAllStringSubmatch(readmeText(t), -1)
	if len(found) == 0 {
		t.Fatalf("README says nothing of the form \"the last N requests\", so nothing here ties its "+
			"prose to requestlog.Capacity (%d). If the sentence moved, move the pattern with it; "+
			"if the number left README, retire this test.", requestlog.Capacity)
	}
	for _, m := range found {
		if m[1] != strconv.Itoa(requestlog.Capacity) {
			t.Errorf("README says %q and requestlog.Capacity is %d: the buffer's size is written down "+
				"in two places and they disagree", m[0], requestlog.Capacity)
		}
	}
}

// lastNRequests is README's prose for [requestlog.Capacity]: "the last 1024
// requests", in the log_level section.
var lastNRequests = regexp.MustCompile(`the last ([0-9]+) requests`)

// TestReadmeNamesTheShippedRateLimit: README's rate_limit paragraph tells a
// reader what an enabled limiter refuses at before they have configured
// anything, and internal/config/layers/base.pxf is the only place that
// decides it (#223) — the same drift #163 gave the request log's capacity a
// check for. Changing the shipped budget or window would otherwise leave
// README quietly wrong.
//
// README alone, again: `changelog.d/added/proxy-rate-limit.md` and ADR 006's
// amendment name the numbers too and keep them, because a released note and
// a dated decision say what shipped then. README is the one that has to be
// true today.
//
// The defaults are read through [config.Load] with no user layer rather than
// from base.pxf's text, so the test pins what voya would actually run with.
func TestReadmeNamesTheShippedRateLimit(t *testing.T) {
	cfg, err := config.Load(t.Context(), "")
	if err != nil {
		t.Fatalf("loading the committed defaults: %v", err)
	}
	rl := cfg.GetRateLimit()

	found := shippedRateLimit.FindStringSubmatch(readmeText(t))
	if found == nil {
		t.Fatalf("README says nothing of the form \"N per <unit> as shipped\", so nothing ties its "+
			"rate_limit prose to base.pxf (%d per %s). If the sentence moved, move the pattern with "+
			"it; if the numbers left README, retire this test.", rl.GetRequests(), rl.GetWindow().AsDuration())
	}
	if want := strconv.FormatUint(uint64(rl.GetRequests()), 10); found[1] != want {
		t.Errorf("README says %q and base.pxf sets rate_limit.requests = %s: the shipped budget is "+
			"written down in two places and they disagree", found[0], want)
	}
	window := rl.GetWindow().AsDuration()
	want, named := windowWords[window]
	switch {
	case !named:
		t.Errorf("base.pxf sets rate_limit.window = %s, which README's %q cannot spell: rewrite the "+
			"sentence and give the new unit a word here", window, found[0])
	case found[2] != want:
		t.Errorf("README says %q and base.pxf sets rate_limit.window = %s: the shipped window is "+
			"written down in two places and they disagree", found[0], window)
	}
}

// shippedRateLimit is README's prose for base.pxf's rate_limit thresholds:
// "5 per minute as shipped", in the configuration section. Whitespace rather
// than a literal space between the words, because the phrase already wraps
// across a line break and a re-wrap must not silently empty this pin.
var shippedRateLimit = regexp.MustCompile(`([0-9]+)\s+per\s+([a-z]+)\s+as shipped`)

// windowWords is the unit README may name a rate_limit window in, for the
// windows that are one of it. A window this map has no word for is a
// sentence that has to be rewritten, not a test that has to be loosened.
var windowWords = map[time.Duration]string{
	time.Second: "second",
	time.Minute: "minute",
	time.Hour:   "hour",
}

// TestReadmeNamesTheRequiredClientTag: README tells a service author which
// `client` tag to `go get`, twice, and says the version is "the version the
// root requires too" — while the root `go.mod`'s require is what actually
// decides it. Nothing tied the two together until this test, and the pair has
// now drifted in lockstep twice (`v0.2.0` in #246, `v0.2.1` in #280), caught
// by a reader both times. This is the same drift #163 gave the request-log
// capacity a check for.
//
// Only the `go get` lines are pinned. README's prose `client/v0.2.1` is a
// dated fact about which tag first carried `proto/v0.2.1`, and it stays true
// as the require moves on; an install command that names a version the root
// does not require is the one that misleads.
func TestReadmeNamesTheRequiredClientTag(t *testing.T) {
	want := requiredClientVersion(t)
	found := clientGoGet.FindAllStringSubmatch(readmeText(t), -1)
	if len(found) == 0 {
		t.Fatalf("README names no `go get %s@<version>`, so nothing here ties its install command to "+
			"go.mod's require of %s: if the command left README, retire this test.", clientPath, want)
	}
	for _, m := range found {
		if m[1] != want {
			t.Errorf("README says `go get %s@%s` and go.mod requires %s: the version a consumer is "+
				"told to fetch is written down twice and the copies disagree.", clientPath, m[1], want)
		}
	}
}

// clientPath is the client module, named once for the messages above and the
// two patterns below.
const clientPath = "github.com/trendvidia/voya/client"

// clientGoGet is README's install command for [clientPath]: the module path
// followed by @ and the tag, wherever it appears. The version runs to the
// first character a tag cannot hold — the closing backtick or the comma that
// ends it in prose — so a pre-release suffix is captured whole rather than
// truncated into a mismatch.
var clientGoGet = regexp.MustCompile(regexp.QuoteMeta(clientPath) + `@(v[0-9][0-9A-Za-z.+-]*)`)

// clientRequire is the root go.mod's require line for [clientPath]. A require
// block entry, so leading whitespace and nothing but the version after it.
var clientRequire = regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(clientPath) + `\s+(v\S+)\s*$`)

// requiredClientVersion is the version the root module requires of
// [clientPath], read from go.mod itself for the same reason readmeText reads
// README: a pin against a fixture proves nothing.
func requiredClientVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	found := clientRequire.FindStringSubmatch(string(raw))
	if found == nil {
		t.Fatalf("go.mod has no require of %s, so there is no version for README to agree with: if "+
			"the root stopped requiring it, retire this test.", clientPath)
	}
	return found[1]
}

// readmeText is README.md, from the repository root two directories up. The
// tests here read the file itself rather than a copy: a check that its prose
// still matches the code is worth nothing against a fixture.
func readmeText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	return string(raw)
}
