// Package gatestest drives the Makefile's gates on a copy of the files they
// read, so a gate's refusals are exercised rather than assumed. A gate that
// never fails looks exactly like a gate that was narrowed until it cannot.
package gatestest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	repoRoot = "../.."

	// Whole lines of the real ci.yml that the cases below rewrite. replaceIn
	// fails the test if one is gone, so a reworded workflow cannot leave a case
	// feeding the gate an unmodified file.
	jobRunsOn    = "    runs-on: ubuntu-latest\n"
	matrixRunsOn = "            runs-on: macos-latest\n"
	exprRunsOn   = "    runs-on: ${{ matrix.runs-on }}\n"

	refusal = "workflow-check: a workflow names a runner this gate cannot show is GitHub-hosted:"
	passed  = "==> workflow-check: "
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// replaceIn rewrites the first line equal to old. It matches from the line's
// start because jobRunsOn is also the tail of the matrix's own ubuntu line, and
// a plain substring would rewrite that one instead.
func replaceIn(t *testing.T, yml, old, replacement string) string {
	t.Helper()
	if !strings.Contains(yml, "\n"+old) {
		t.Fatalf("ci.yml no longer has the line %q, so this case would test the unmodified file", old)
	}
	return strings.Replace(yml, "\n"+old, "\n"+replacement, 1)
}

// runWorkflowCheck runs `make workflow-check` in a directory holding the real
// Makefile and the workflows given, which is all that recipe reads. It returns
// what make said, and whether make exited zero. Any failure other than a
// non-zero exit fails the test, so a make that never ran is not a refusal.
func runWorkflowCheck(t *testing.T, workflows map[string]string) (string, bool) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not on PATH")
	}

	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(read(t, "Makefile")), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range workflows {
		if err := os.WriteFile(filepath.Join(wf, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", "-C", dir, "workflow-check")
	cmd.Env = withoutMakeState(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return out.String(), true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("make did not run: %v\n%s", err, out.String())
	}
	return out.String(), false
}

// withoutMakeState drops what an outer make hands its children. Under `make
// test` this test is one, and a flag or jobserver inherited from there is not
// part of what is being tested.
func withoutMakeState(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "MAKEFLAGS", "MFLAGS", "MAKELEVEL", "MAKEOVERRIDES", "GNUMAKEFLAGS":
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// TestWorkflowCheckPassesTheRealFile is the done-when's second half, and the
// control for every refusal below: the same recipe on the same Makefile passes
// until a case changes the workflow. The count is taken here a second way, so
// a scan that quietly reads fewer lines fails rather than passing on less.
func TestWorkflowCheckPassesTheRealFile(t *testing.T) {
	yml := read(t, ".github/workflows/ci.yml")

	want := 0
	for line := range strings.Lines(yml) {
		if strings.Contains(line, "runs-on") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			want++
		}
	}
	if want == 0 {
		t.Fatal("found no runs-on line in the real ci.yml, so this test proves nothing")
	}

	out, ok := runWorkflowCheck(t, map[string]string{"ci.yml": yml})
	if !ok {
		t.Fatalf("the real ci.yml was refused:\n%s", out)
	}
	if marker := passed + strconv.Itoa(want) + " runs-on line(s)"; !strings.Contains(out, marker) {
		t.Errorf("want %q in the output, which proves the scan read every line:\n%s", marker, out)
	}
}

// TestWorkflowCheckRefusesRunnersItCannotShowAreHosted is the done-when's first
// half, widened to each way a job reaches a runner that is not GitHub's. Most
// of these never say `self-hosted`, which is why the gate is an allowlist.
func TestWorkflowCheckRefusesRunnersItCannotShowAreHosted(t *testing.T) {
	yml := read(t, ".github/workflows/ci.yml")

	tests := []struct {
		name, old, replacement string
		offending              string // what the refusal must quote
	}{
		{
			name: "a job's label", old: jobRunsOn,
			replacement: "    runs-on: self-hosted\n",
			offending:   "runs-on: self-hosted",
		},
		{
			name: "a flow list", old: jobRunsOn,
			replacement: "    runs-on: [self-hosted, linux, x64]\n",
			offending:   "runs-on: [self-hosted, linux, x64]",
		},
		{
			name: "a block list", old: jobRunsOn,
			replacement: "    runs-on:\n      - self-hosted\n      - linux\n",
			offending:   "runs-on:",
		},
		{
			name: "a matrix entry", old: matrixRunsOn,
			replacement: "            runs-on: [self-hosted, macOS]\n",
			offending:   "runs-on: [self-hosted, macOS]",
		},
		{
			name: "a matrix axis the gate does not read", old: exprRunsOn,
			replacement: "    runs-on: ${{ matrix.os }}\n",
			offending:   "runs-on: ${{ matrix.os }}",
		},
		{
			name: "a flow mapping in a matrix", old: matrixRunsOn,
			replacement: "            runs-on: macos-latest\n          - {name: extra, runs-on: self-hosted}\n",
			offending:   "{name: extra, runs-on: self-hosted}",
		},
		{
			name: "a custom label that never says self-hosted", old: jobRunsOn,
			replacement: "    runs-on: voya\n",
			offending:   "runs-on: voya",
		},
		{
			name: "a custom label that starts like a hosted one", old: jobRunsOn,
			replacement: "    runs-on: ubuntu-gpu\n",
			offending:   "runs-on: ubuntu-gpu",
		},
		{
			name: "a custom label that extends a hosted one", old: jobRunsOn,
			replacement: "    runs-on: macos-latest-trader\n",
			offending:   "runs-on: macos-latest-trader",
		},
		{
			name: "custom labels in a list", old: jobRunsOn,
			replacement: "    runs-on: [voya, Linux]\n",
			offending:   "runs-on: [voya, Linux]",
		},
		{
			name: "a hosted name inside a list", old: jobRunsOn,
			replacement: "    runs-on: [ubuntu-latest, voya]\n",
			offending:   "runs-on: [ubuntu-latest, voya]",
		},
		{
			name: "a runner group as a block", old: jobRunsOn,
			replacement: "    runs-on:\n      group: trendvidia\n      labels: [linux]\n",
			offending:   "runs-on:",
		},
		{
			name: "a runner group inline", old: jobRunsOn,
			replacement: "    runs-on: {group: trendvidia}\n",
			offending:   "runs-on: {group: trendvidia}",
		},
		{
			name: "an expression building the labels", old: jobRunsOn,
			replacement: "    runs-on: ${{ fromJSON('[\"self-hosted\", \"linux\"]') }}\n",
			offending:   "fromJSON",
		},
		{
			name: "an expression choosing a runner", old: jobRunsOn,
			replacement: "    runs-on: ${{ github.event_name == 'push' && 'self-hosted' || 'ubuntu-latest' }}\n",
			offending:   "github.event_name",
		},
		{
			name: "a trailing comment that says it is hosted", old: jobRunsOn,
			replacement: "    runs-on: self-hosted # ubuntu-latest\n",
			offending:   "runs-on: self-hosted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := runWorkflowCheck(t, map[string]string{
				"ci.yml": replaceIn(t, yml, tt.old, tt.replacement),
			})
			if ok {
				t.Fatalf("passed a workflow holding %q:\n%s", tt.replacement, out)
			}
			// That make failed is not the claim. The refusal has to be this
			// one, and has to quote the line, or a broken recipe would pass.
			if !strings.Contains(out, refusal) {
				t.Errorf("failed, but not with the runner refusal:\n%s", out)
			}
			if !strings.Contains(out, "ci.yml:") || !strings.Contains(out, tt.offending) {
				t.Errorf("the refusal does not quote ci.yml's line %q:\n%s", tt.offending, out)
			}
		})
	}
}

// TestWorkflowCheckReadsEveryWorkflow covers a new file. It needs no edit to
// ci.yml to reach a runner, so a gate reading ci.yml alone would pass it.
func TestWorkflowCheckReadsEveryWorkflow(t *testing.T) {
	const other = "on: push\njobs:\n  deploy:\n    runs-on: self-hosted\n    steps:\n      - run: \"true\"\n"
	yml := read(t, ".github/workflows/ci.yml")

	for _, name := range []string{"deploy.yml", "deploy.yaml"} {
		t.Run(name, func(t *testing.T) {
			out, ok := runWorkflowCheck(t, map[string]string{"ci.yml": yml, name: other})
			if ok {
				t.Fatalf("passed %s, which asks for a self-hosted runner:\n%s", name, out)
			}
			if !strings.Contains(out, refusal) || !strings.Contains(out, name+":4:") {
				t.Errorf("want the runner refusal, naming %s line 4:\n%s", name, out)
			}
		})
	}
}

// TestWorkflowCheckAcceptsHostedRunners keeps the allowlist from being a list
// of today's two labels, and keeps prose out of the scan: the comment in
// ci.yml that forbids a self-hosted runner must be free to name one.
func TestWorkflowCheckAcceptsHostedRunners(t *testing.T) {
	yml := read(t, ".github/workflows/ci.yml")

	tests := []struct{ name, old, replacement string }{
		{"a pinned ubuntu", jobRunsOn, "    runs-on: ubuntu-24.04\n"},
		{"an arm ubuntu", jobRunsOn, "    runs-on: ubuntu-24.04-arm\n"},
		{"a pinned macos", matrixRunsOn, "            runs-on: macos-15\n"},
		{"a sized macos", matrixRunsOn, "            runs-on: macos-15-xlarge\n"},
		{"windows", jobRunsOn, "    runs-on: windows-latest\n"},
		{"an arm windows", jobRunsOn, "    runs-on: windows-11-arm\n"},
		{"a whole-line comment", jobRunsOn, "    # runs-on: self-hosted\n" + jobRunsOn},
		{"a trailing comment", jobRunsOn, "    runs-on: ubuntu-latest # not runs-on: self-hosted\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := runWorkflowCheck(t, map[string]string{
				"ci.yml": replaceIn(t, yml, tt.old, tt.replacement),
			})
			if !ok {
				t.Fatalf("refused a hosted runner, %q:\n%s", tt.replacement, out)
			}
			if !strings.Contains(out, passed) {
				t.Errorf("exited zero without the line saying the scan ran:\n%s", out)
			}
		})
	}
}

// TestWorkflowCheckRefusesWhenItReadNothing holds the gate to the Makefile's
// rule that parsing nothing is the gate breaking. Otherwise a scan that stopped
// matching would pass every workflow there is.
func TestWorkflowCheckRefusesWhenItReadNothing(t *testing.T) {
	var kept strings.Builder
	for line := range strings.Lines(read(t, ".github/workflows/ci.yml")) {
		if !strings.Contains(line, "runs-on") {
			kept.WriteString(line)
		}
	}

	out, ok := runWorkflowCheck(t, map[string]string{"ci.yml": kept.String()})
	if ok {
		t.Fatalf("passed a workflow set with no runs-on at all:\n%s", out)
	}
	if !strings.Contains(out, "workflow-check: found no runs-on") {
		t.Errorf("failed, but not by saying it found nothing:\n%s", out)
	}
}
