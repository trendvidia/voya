package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"google.golang.org/protobuf/proto"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/diag"
	"github.com/trendvidia/voya/internal/lsof"
	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// diagDeadline bounds one unary DiagService call from a CLI: the daemon
// answers every one of them from memory across loopback (ADR 005), so an
// answer that has not arrived in this long is a wedged voya, and a hung voya
// is a different diagnosis from an absent one — [notRunning] tells them
// apart by exit code. Two seconds is generous by orders of magnitude against
// a copy of a thousand records; what it buys is that a `voya log` in a shell
// pipeline against a stuck daemon ends with a sentence rather than hanging.
const diagDeadline = 2 * time.Second

// diagTarget is a running voya as a CLI reaches it: DiagService dialled on
// the proxy port under the reserved authority — the same path a service's
// call takes, so a working `voya log` also proves the routing (#149).
type diagTarget struct {
	proxyPort uint32
	client    diagv1connect.DiagServiceClient
	transport *http.Transport
}

// dialProxy resolves the same configuration `voya run` reads, so a developer
// who moved proxy_port gets the voya they run, and returns that port with a
// transport aimed at it. Nothing is dialled yet: the first call is what finds
// out whether voya is there.
//
// h2c, and a dialer that goes to 127.0.0.1:port whatever the URL names —
// which is what the client package's dialer does for a *.internal name. It
// is restated here rather than imported because that package is what
// services link, and a CLI-side dependency on it would make every change to
// the CLI's transport a change to theirs.
//
// The URL's authority is therefore the caller's to choose, and it is the
// whole of what routing keys on: [dialDiag] names the reserved voya.internal
// to read the daemon, and `voya call` names a service's own so its request
// enters voya the way that service's callers' do (#158).
func dialProxy(ctx context.Context) (port uint32, tr *http.Transport, err error) {
	cfg, err := config.Load(ctx, config.Dir())
	if err != nil {
		return 0, nil, err
	}
	port = cfg.GetProxyPort()
	addr := net.JoinHostPort(config.Loopback, strconv.FormatUint(uint64(port), 10))
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return port, &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}, nil
}

// dialDiag is [dialProxy] aimed at DiagService under the reserved authority
// — the same path a service's call takes, so a working `voya log` also
// proves the routing (#149).
func dialDiag(ctx context.Context) (*diagTarget, error) {
	port, tr, err := dialProxy(ctx)
	if err != nil {
		return nil, err
	}
	url := "http://" + registry.Authority(registry.Reserved) + ":" + strconv.FormatUint(uint64(port), 10)
	return &diagTarget{
		proxyPort: port,
		client:    diagv1connect.NewDiagServiceClient(&http.Client{Transport: tr}, url),
		transport: tr,
	}, nil
}

// close lets go of the pooled connection, so a follower that was interrupted
// does not leave one open on the way out.
func (d *diagTarget) close() { d.transport.CloseIdleConnections() }

// notRunning turns the failure of a call through the proxy into the answer a
// CLI prints and the code it exits with, or reports that err is neither. The
// refusal is [diag.ConnectionRefused] and not a bare errors.Is, for the
// reason that function gives: on Windows the sentinel matches nothing a
// socket returns, and this row is the one every M2 command's absent-voya
// answer comes from.
//
// "voya is not running on port N" is an *answer*, not a failure to render:
// nothing accepted the connection, so one line saying so and what to run,
// exit 1. A deadline that expired is a different diagnosis — something is
// listening and did not answer — so it is a different line and exit 2 (ADR
// 005 makes `status` and its neighbours the commands reached for when
// something is already wrong; a hung voya must not read as an absent one).
//
// within is the deadline that expired, named in that second line, because
// the commands do not share one: a DiagService call is bounded by
// [diagDeadline] and one `voya call` by [callDeadline], and a line quoting
// the other command's figure is a diagnostic that sends the reader to check
// the wrong number.
func notRunning(port uint32, within time.Duration, err error) (line string, code int, ok bool) {
	switch {
	case diag.ConnectionRefused(err):
		return fmt.Sprintf("voya is not running on port %d (connection refused); start it with `voya run`", port), 1, true
	case errors.Is(err, context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded:
		return fmt.Sprintf("voya on port %d did not answer within %s", port, within), 2, true
	}
	return "", 0, false
}

// notRunning is [notRunning] for a DiagService call, under its deadline.
func (d *diagTarget) notRunning(err error) (line string, code int, ok bool) {
	return notRunning(d.proxyPort, diagDeadline, err)
}

// transportFailed reports whether a call failed with no response at all:
// Connect's Unavailable with no response headers. The headers are what tell
// this from a daemon saying Unavailable itself, as it does when `voya log -f`
// outlives it. A proxy-routed `voya call` gets Unavailable from a service, so
// this row is for DiagService calls alone.
func transportFailed(err error) bool {
	var cerr *connect.Error
	return errors.As(err, &cerr) && cerr.Code() == connect.CodeUnavailable && len(cerr.Meta()) == 0
}

// otherOnPort words a port held by something that is not voya the way `voya
// run`'s refusal does, naming the holder when lsof can.
func otherOnPort(ctx context.Context, port uint32, err error) string {
	line := fmt.Sprintf("voya: port %d (proxy) is held by something that does not answer as voya does (%v)", port, connectMessage(err))
	if holder := lsof.Holder(ctx, lsof.TCP, port); holder != "" {
		line += "\n  held by: " + holder
	}
	return line + "\n  voya needs this port; stop the other process, or change it in your voya.pxf"
}

// unary makes one DiagService call under [diagDeadline] and turns the two
// answers that are not failures into what every M2 command prints for them:
// a voya that is not there, and a voya that accepts and never answers, each
// as one line with the verdict in the exit code — [notRunning]'s two — and
// anything else as the daemon's own words on stderr, exit 1. The call is the
// client method itself, so `status`, `routes` and `log`'s snapshot share one
// copy of the deadline, the diagnosis and the streams, and
// TestDiagCommandsWhenVoyaIsNotRunning pins one behaviour rather than three.
//
// prose is where those two lines go, and every caller gets it from [prose]:
// stdout in the human form, stderr under -pxf.
func unary[Req, Res any](ctx context.Context, target *diagTarget, prose io.Writer,
	call func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), req *Req,
) (*Res, error) {
	callCtx, cancel := context.WithTimeout(ctx, diagDeadline)
	defer cancel()
	res, err := call(callCtx, connect.NewRequest(req))
	if err != nil {
		if line, code, ok := target.notRunning(err); ok {
			fmt.Fprintln(prose, line)
			return nil, &exitStatus{code: code}
		}
		if transportFailed(err) {
			fmt.Fprintln(prose, otherOnPort(ctx, target.proxyPort, err))
			return nil, &exitStatus{code: 1}
		}
		return nil, &exitStatus{code: 1, err: connectMessage(err)}
	}
	return res.Msg, nil
}

// prose is where a command's own words go rather than its product: stdout in
// the human form, where the sentence *is* the product (#151), and stderr
// under -pxf, where stdout carries one document or nothing at all.
//
// One rule, and one function, for every such line the commands sharing
// [unary] write: the absent-voya answer and the deadline line from
// [notRunning], `routes`' "no services registered" and `log`'s "no requests
// yet". Decided once for the three in #163 and #166 — a pxf reader cannot
// parse an English sentence, and the exit code (1 refused, 2 hung) is already
// the machine-readable verdict, so the stream stays a document or empty.
func prose(asPXF bool, stdout, stderr io.Writer) io.Writer {
	if asPXF {
		return stderr
	}
	return stdout
}

// pxfFlag parses the one flag `routes` and `health` take, -pxf,
// and refuses positional words the way `log` does: usage on stderr and the
// error, or usage on stdout and done for -h. name is the command's own, for
// the flag set and the refusal.
//
// printUsage is the command's own usage function and not main's [usage],
// which it would otherwise shadow: the two are one identifier apart, and the
// wrong one here answers `voya status -h` with the whole CLI's help.
func pxfFlag(name string, printUsage func(io.Writer), args []string, stdout, stderr io.Writer) (asPXF, done bool, err error) {
	return pxfFlags(name, printUsage, args, stdout, stderr, nil)
}

// pxfFlags is [pxfFlag] for a command that takes -pxf and more: define adds
// the command's other flags to the set before it is parsed, so `status`,
// which also takes -health, shares this one parse and refusal rather than
// carrying a second copy of them. define may be nil.
func pxfFlags(name string, printUsage func(io.Writer), args []string, stdout, stderr io.Writer,
	define func(*flag.FlagSet),
) (asPXF, done bool, err error) {
	var asPXFFlag *bool
	done, err = parseFlags(name, printUsage, args, stdout, stderr, func(fs *flag.FlagSet) {
		asPXFFlag = fs.Bool("pxf", false, "render the answer as pxf")
		if define != nil {
			define(fs)
		}
	})
	if done {
		return false, true, err
	}
	return *asPXFFlag, false, nil
}

// parseFlags is the argument contract the whole CLI answers by, in one place
// because it is asserted command by command: -h prints the command's usage on
// stdout and is done, exit 0; an unknown flag or a positional word prints it
// on stderr and fails with a message naming what was typed. define adds the
// command's own flags to the set before it is parsed — -pxf for the commands
// [pxfFlags] serves, --activate for the two `dns` ones — so a command that
// takes a different flag does not carry a second copy of the contract, which
// is how the two wordings would drift apart.
func parseFlags(name string, printUsage func(io.Writer), args []string, stdout, stderr io.Writer,
	define func(*flag.FlagSet),
) (done bool, err error) {
	fs := flag.NewFlagSet("voya "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	define(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return true, nil
		}
		printUsage(stderr)
		return true, err
	}
	if fs.NArg() != 0 {
		printUsage(stderr)
		return true, fmt.Errorf("%s takes no arguments, got %q", name, strings.Join(fs.Args(), " "))
	}
	return false, nil
}

// pxfDocument writes m as one whole pxf document — pxf.Marshal's own
// multi-line form, unfolded — which is what `status` and `routes` print
// under -pxf: one answer is not a stream, so unlike [pxfLine] nothing here
// is squeezed onto a line. One copy for both commands, beside [unary] and
// [pxfFlag], because #152 asked whichever command came second to share the
// client rather than copy it, and a rendering the two disagreed about would
// be the same drift one function lower.
func pxfDocument(w io.Writer, m proto.Message) error {
	b, err := pxf.Marshal(m)
	if err != nil {
		return fmt.Errorf("rendering pxf: %w", err)
	}
	_, err = w.Write(b)
	return err
}
