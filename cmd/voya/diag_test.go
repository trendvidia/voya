package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/trendvidia/voya/internal/config"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// --- the DiagService clients share their two not-a-failure answers ------------
//
// `log`, `status`, `routes` and `health` reach the daemon through [unary] over
// [diagTarget.notRunning], so a voya that is not there and a voya that accepts
// and never answers are one behaviour, pinned once for all three rather than
// three times: #151 specified it, #159 landed it first, and #152 said whoever
// came second shares it. If a command ever stops going through [unary], the
// subtest with its name is what says so.

// diagCommands is every command that makes a unary DiagService call as its
// first act, and so answers the two diagnoses with the code.
// `health` is `status` with the probes after the same GetStatus (#154), so a
// voya that is not there ends it before any probe.
var diagCommands = []string{"log", "status", "routes", "health"}

// pxfStates is the two forms every one of those commands answers in, and the
// stream each puts a not-a-failure answer on: stdout in the human form, where
// the sentence *is* the product (#151), and stderr under -pxf, where stdout
// carries one document or nothing at all (#163 and #166, decided together for
// the commands sharing [unary] — see [prose]).
var pxfStates = []struct {
	name  string
	args  []string
	asPXF bool
}{
	{name: "plain"},
	{name: "pxf", args: []string{"-pxf"}, asPXF: true},
}

// TestDiagCommandsWhenVoyaIsNotRunning: nothing on the port is an answer —
// one line naming the port and `voya run`, exit 1 — and a listener that
// accepts and never answers is a different one: the deadline line, exit 2. For
// each of the four commands, in both flag states, against one stopped daemon's
// ports. The exit code is the same either way; which stream the line lands on
// is not, and the other stream is asserted empty — under -pxf that is the
// whole promise, because a pipe reading the document reads nothing else.
func TestDiagCommandsWhenVoyaIsNotRunning(t *testing.T) {
	skipWithoutSIGTERM(t)
	// A daemon that is not there: a config directory naming ports nothing
	// holds, without a `voya run` on them.
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	v.Signal(t, syscall.SIGTERM)
	v.WaitForExit(t, "after SIGTERM", shutdownBound)

	for _, name := range diagCommands {
		for _, st := range pxfStates {
			t.Run(name+"/absent/"+st.name, func(t *testing.T) {
				absent := startVoyaCmd(t, v, name, st.args...)
				if code := absent.exitCode(t, "with no daemon"); code != 1 {
					t.Errorf("`voya %s` with no daemon exited %d, want 1\n%s", name, code, absent.Output())
				}
				want := fmt.Sprintf("voya is not running on port %d", v.ports.Proxy)
				answer, stream, quiet := answerStream(absent, st.asPXF)
				if !strings.Contains(answer, want) || !strings.Contains(answer, "voya run") {
					t.Errorf("`voya %s %s` with no daemon printed %q on %s, want %q and what to run",
						name, strings.Join(st.args, " "), answer, stream, want)
				}
				if quiet != "" {
					t.Errorf("`voya %s %s` with no daemon left %q on the other stream, want it clean",
						name, strings.Join(st.args, " "), quiet)
				}
			})
		}
	}

	// A wedged one: something accepts on the proxy port and never answers.
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.FormatUint(uint64(v.ports.Proxy), 10))
	if err != nil {
		t.Skipf("the stopped daemon's proxy port was taken in between: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	for _, name := range diagCommands {
		for _, st := range pxfStates {
			t.Run(name+"/hung/"+st.name, func(t *testing.T) {
				hung := startVoyaCmd(t, v, name, st.args...)
				if code := hung.exitCode(t, "against a listener that never answers"); code != 2 {
					t.Errorf("`voya %s` against a listener that never answers exited %d, want 2\n%s", name, code, hung.Output())
				}
				answer, stream, quiet := answerStream(hung, st.asPXF)
				if !strings.Contains(answer, "did not answer within") {
					t.Errorf("`voya %s %s` against a listener that never answers printed %q on %s, want the deadline line",
						name, strings.Join(st.args, " "), answer, stream)
				}
				if quiet != "" {
					t.Errorf("`voya %s %s` against a listener that never answers left %q on the other stream, want it clean",
						name, strings.Join(st.args, " "), quiet)
				}
			})
		}
	}
}

// answerStream is where a not-a-failure answer belongs in each form and what
// the other stream held: stdout and stderr in the human form, and the two
// swapped under -pxf. Returned with the stream's name, so a failure message
// says which one it read.
func answerStream(c *voyaCmd, asPXF bool) (answer, stream, quiet string) {
	if asPXF {
		return c.Stderr.String(), "stderr", c.Stdout.String()
	}
	return c.Stdout.String(), "stdout", c.Stderr.String()
}

// TestPXFFlagCommandsRejectWhatTheyDoNotTake drives [pxfFlag] through
// dispatch for `status`, `routes` and `health`: positional words
// and unknown flags are refused with the command's usage on stderr, and -h is
// help on stdout — without a daemon, since parsing comes before the dial.
// `status` takes -health besides, through [pxfFlags], and its usage line says
// so.
func TestPXFFlagCommandsRejectWhatTheyDoNotTake(t *testing.T) {
	for name, usageLine := range map[string]string{
		"status": "voya status [-health] [-pxf]",
		"routes": "voya routes [-pxf]",
		"health": "voya health [-pxf]",
	} {
		for _, tc := range []struct {
			args    []string
			wantErr string
		}{
			{[]string{name, "junk"}, name + ` takes no arguments, got "junk"`},
			{[]string{name, "-bogus"}, "flag provided but not defined: -bogus"},
			{[]string{name, "-pxf", "junk"}, name + ` takes no arguments, got "junk"`},
		} {
			t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				err := run(tc.args, &stdout, &stderr)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("run(%q) = %v, want %q", tc.args, err, tc.wantErr)
				}
				if !strings.Contains(stderr.String(), usageLine) {
					t.Errorf("no %s usage on stderr:\n%s", name, stderr.String())
				}
				if stdout.Len() != 0 {
					t.Errorf("run(%q) wrote to stdout: %q", tc.args, stdout.String())
				}
			})
		}
		t.Run(name+"_-h", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run([]string{name, "-h"}, &stdout, &stderr); err != nil {
				t.Errorf("`voya %s -h`: %v", name, err)
			}
			if !strings.Contains(stdout.String(), usageLine) || stderr.Len() != 0 {
				t.Errorf("`voya %s -h` printed stdout %q stderr %q", name, stdout.String(), stderr.String())
			}
		})
	}
}

// TestDiagCommandsNameAnotherProgramOnThePort is the row #314 added when the
// proxy-port check went: something on the port that answers, and not as voya
// does, is said in those words with its holder named, rather than the
// transport's "http2: frame too large". The listener answers HTTP/1.1, as a
// stray web server does, which is what produced that message.
func TestDiagCommandsNameAnotherProgramOnThePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
			_ = conn.Close()
		}
	}()
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	dir := t.TempDir()
	layer := fmt.Sprintf("proxy_port = %d\n", port)
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte(layer), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}
	t.Setenv(config.DirEnv, dir)

	for _, name := range diagCommands {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run([]string{name}, &stdout, &stderr)
			var status *exitStatus
			if !errors.As(err, &status) || status.code != 1 {
				t.Fatalf("`voya %s` = %v, want exit 1", name, err)
			}
			out := stdout.String()
			for _, want := range []string{
				fmt.Sprintf("port %d (proxy) is held by something that does not answer as voya does", port),
				"voya needs this port",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("`voya %s` does not say %q:\n%s", name, want, out)
				}
			}
			if _, lookErr := exec.LookPath("lsof"); lookErr == nil {
				if want := "(pid " + strconv.Itoa(os.Getpid()) + ")"; !strings.Contains(out, want) {
					t.Errorf("lsof is on this host, but `voya %s` does not name this process %q:\n%s", name, want, out)
				}
			}
		})
	}
}

// answersUnavailable is a voya whose GetStatus answers Unavailable itself.
type answersUnavailable struct {
	diagv1connect.UnimplementedDiagServiceHandler
}

func (answersUnavailable) GetStatus(context.Context, *connect.Request[diagv1.GetStatusRequest]) (*connect.Response[diagv1.GetStatusResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("voya is going away"))
}

// TestAnUnavailableVoyaSaysSoItself is the other half of the row above: an
// Unavailable that arrived in a response came from voya, so its words are
// printed and no other program is blamed for the port.
func TestAnUnavailableVoyaSaysSoItself(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	path, handler := diagv1connect.NewDiagServiceHandler(answersUnavailable{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	dir := t.TempDir()
	layer := fmt.Sprintf("proxy_port = %d\n", ln.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte(layer), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}
	t.Setenv(config.DirEnv, dir)

	var stdout, stderr bytes.Buffer
	err = run([]string{"status"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "voya is going away") {
		t.Fatalf("`voya status` = %v, want the daemon's own words", err)
	}
	if strings.Contains(stdout.String(), "held by") {
		t.Errorf("`voya status` blamed another program for voya's own answer:\n%s", stdout.String())
	}
}
