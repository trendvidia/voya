// Package config resolves voya's own configuration: a typed
// voya.config.v1.Config, merged by chameleon from layered .pxf files
// (ADR 001).
//
// There are two layers. The first is base.pxf, committed next to this file
// and embedded in the binary — it sets every field, and it is the only place
// the default values are written down. The second is voya.pxf in the
// developer's config directory, which is optional and normally absent.
//
// That ordering is the whole design: a machine with no configuration at all
// resolves to the committed defaults rather than to an error, because a dev
// tool that needs a config file before it will start has already failed at
// being a dev tool.
//
// ADR 001 says to start with chameleon.Load and reach for resolver.New only
// if multiple sources or encrypted layers appear. Multiple sources appeared
// at once: base.pxf is read from the embedded FS and voya.pxf from the
// developer's config directory, whereas chameleon.Load takes a single layer
// FS for both. Keeping them apart is also what makes a stray base.pxf in the
// config directory inert. resolver.New is the only way to ask for the
// per-field trace besides, which is what lets a validation failure name the
// layer and file that set the offending value. Nothing is paid in link weight
// for the choice: chameleon.Load returns a resolver.Resolution, so either way
// this package pulls in chameleon/resolver.
//
// Since chameleon v0.23.0 that is all it pulls in. Decryption sits behind a
// crypt.Decrypter that a backend registers, and voya registers none — so
// resolving these two plaintext layers links no key management at all, where
// v0.22.1's resolver imported chameleon/sops at package level and brought AWS,
// Azure, GCP, Vault and Huawei with it (issue #28). deps_test.go pins that;
// registering a decrypter is what would undo it.
package config

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/trendvidia/chameleon/chain"
	"github.com/trendvidia/chameleon/resolver"
	"github.com/trendvidia/chameleon/source"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// The chain spec and the committed default layer travel inside the binary:
// `go install github.com/trendvidia/voya/cmd/voya` is the documented way to
// get voya (ADR 001), and it copies no files anywhere.
//
//go:embed layers/chain.pxf layers/base.pxf
var embedded embed.FS

const (
	// UserFile is the optional layer voya reads from [Dir]. The name is
	// fixed rather than configurable: the chain spec that references it is
	// embedded, so a configurable name would have to be known before the
	// configuration is read.
	UserFile = "voya.pxf"

	// DirEnv overrides [Dir]. Useful for running two voya instances against
	// separate configurations, and for tests that must not read the
	// developer's real config.
	DirEnv = "VOYA_CONFIG_DIR"

	// Loopback is the only address voya's listeners ever bind. Not
	// configurable: registration is unauthenticated and trust-on-first-use,
	// and loopback is what bounds it (ADR 002).
	Loopback = "127.0.0.1"

	// environment is the single environment declared in chain.pxf. voya is a
	// laptop tool; the laptop is the environment.
	environment = "local"

	// Chain layer ids, as declared in layers/chain.pxf. They appear in
	// chameleon's own error messages, so error text here has to agree with
	// them — and [LoadOrigin] hands one back, so a caller that wants to say
	// which of the two settled a field has a name to compare against.
	BaseLayer = "base"
	UserLayer = "user"

	// Source names bound in [Load]. "default" is not a choice — chameleon
	// requires a source by that name for layer entries that omit one.
	defaultSource = "default"
	userSource    = "user"
)

// ErrInvalid marks a configuration voya will not run with — a port outside
// the usable range, two listeners on one port, a missing shutdown grace.
// Distinct from a resolution failure, which means the layers could not be
// read or parsed at all.
var ErrInvalid = errors.New("invalid configuration")

// Load resolves the configuration, using dir as the directory that may hold
// the optional user layer. An empty dir means "no user layer" — [Load] then
// returns the committed defaults. Call [Dir] for the conventional location.
//
// The returned Config has been validated: every caller can bind its ports
// and use its durations without re-checking them.
func Load(ctx context.Context, dir string) (*configv1.Config, error) {
	cfg, _, err := LoadOrigin(ctx, dir, "")
	return cfg, err
}

// LoadOrigin is [Load] and, for the one field named, the id of the chain
// layer whose value for it survived: [UserLayer] when the developer's own
// [UserFile] set it, [BaseLayer] when the committed default did. An empty
// field asks nothing and answers "", as does a field no layer set — which
// while base.pxf covers the whole schema (its own test) means a name that is
// not a field at all.
//
// It exists for `voya dns install`, which runs under sudo: the configuration
// directory is then whatever $HOME resolved to for root, and a line naming
// the layer that set dns_port is the difference between a port the developer
// can check and a default they had no reason to suspect (#172).
func LoadOrigin(ctx context.Context, dir, field string) (*configv1.Config, string, error) {
	res, err := resolve(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	// No field of Config is pxf.Secret, so there are no enclaves to wipe —
	// but Destroy is the API's contract, and calling it keeps that true if a
	// secret ever does appear in the schema. It does not touch Config.
	defer res.Destroy()

	if err := validate(res.Config, func(field string) string {
		return origin(res.Trace, field, dir)
	}); err != nil {
		return nil, "", err
	}
	return res.Config, layerOf(res.Trace, field), nil
}

// layerOf is the id of the layer that set field, as [LoadOrigin] reports it.
// A value that came from a schema default belongs to no layer and answers ""
// — voya's schema carries no (pxf.default) annotations, so the trace can say
// it but this repo's configuration cannot produce it.
func layerOf(trace *resolver.Trace, field string) string {
	if trace == nil || field == "" {
		return ""
	}
	entry, ok := trace.Entries[field]
	if !ok || entry.FromDefault {
		return ""
	}
	return entry.LayerID
}

// resolve runs the chameleon pipeline and returns the whole Resolution,
// trace included. Load discards everything but the config; the tests use
// the trace to assert that the committed base layer really does set every
// field of the schema.
func resolve(ctx context.Context, dir string) (*resolver.Resolution[*configv1.Config], error) {
	layersFS, err := fs.Sub(embedded, "layers")
	if err != nil {
		return nil, fmt.Errorf("config: embedded layers: %w", err)
	}
	specData, err := fs.ReadFile(embedded, "layers/chain.pxf")
	if err != nil {
		return nil, fmt.Errorf("config: embedded chain spec: %w", err)
	}
	spec, err := chain.ParseSpec(specData)
	if err != nil {
		return nil, fmt.Errorf("config: embedded chain spec: %w", err)
	}

	r, err := resolver.New[*configv1.Config](&configv1.Config{}, spec,
		resolver.WithSource(defaultSource, source.FS(layersFS)),
		resolver.WithSource(userSource, source.FS(userFS(dir))),
		// Trace is what lets a validation failure name the layer that set
		// the offending value, which is the difference between "invalid
		// config" and "you set this, in this file".
		resolver.WithTrace(),
	)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	res, err := r.Resolve(ctx, resolver.Request{Environment: environment})
	if err != nil {
		// chameleon names the layer id ("base" / "user") and, for a parse
		// failure, the position and field within it. What it cannot know is
		// which file on this machine that id resolved to.
		return nil, &ResolveError{Err: err, Layers: layerPaths(dir)}
	}
	return res, nil
}

// Dir reports the directory voya reads [UserFile] from: $VOYA_CONFIG_DIR if
// set, else $XDG_CONFIG_HOME/voya, else ~/.config/voya. Returns "" when the
// home directory cannot be determined, which [Load] reads as "no user
// layer" rather than as a failure.
//
// os.UserConfigDir is deliberately not used on macOS, where it answers
// ~/Library/Application Support. Nobody hand-edits a .pxf file there, and
// voya's config is meant to be hand-edited.
func Dir() string {
	if d := os.Getenv(DirEnv); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "voya")
	}
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		return filepath.Join(d, "voya")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "voya")
}

// Addr renders a listener address for port. Every voya listener binds
// loopback explicitly and never 0.0.0.0 (ADR 002); this is where that
// decision lives, so no caller has to remember it.
func Addr(port uint32) string {
	return Loopback + ":" + strconv.FormatUint(uint64(port), 10)
}

// userFS backs the optional user layer. An unset directory gets an FS with
// nothing in it rather than os.DirFS(""), which would resolve voya.pxf
// against the process working directory and pick up a stray file.
func userFS(dir string) fs.FS {
	if dir == "" {
		return emptyFS{}
	}
	return os.DirFS(dir)
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// layerPaths maps each chain layer id to where it came from on this
// machine, for error messages.
func layerPaths(dir string) map[string]string {
	user := "not read (no config directory)"
	if dir != "" {
		user = filepath.Join(dir, UserFile)
	}
	return map[string]string{
		BaseLayer: "built into the binary",
		UserLayer: user,
	}
}

// ResolveError reports layers that could not be read, parsed, or merged. It
// carries the layer-id-to-file table because chameleon's own message names
// the layer id alone, and "layer \"user\"" is not something a developer can
// go and open.
type ResolveError struct {
	Err    error
	Layers map[string]string
}

func (e *ResolveError) Error() string {
	var b strings.Builder
	b.WriteString("config: ")
	b.WriteString(e.Err.Error())
	for _, id := range []string{BaseLayer, UserLayer} {
		if path, ok := e.Layers[id]; ok {
			fmt.Fprintf(&b, "\n  layer %q = %s", id, path)
		}
	}
	return b.String()
}

func (e *ResolveError) Unwrap() error { return e.Err }

// origin renders where a field's final value came from, for validation
// messages. Trace is per-field provenance from chameleon's resolve pass.
func origin(trace *resolver.Trace, field, dir string) string {
	if trace == nil {
		return ""
	}
	entry, ok := trace.Entries[field]
	if !ok {
		return "not set by any layer"
	}
	if entry.FromDefault || entry.LayerID == "" {
		// Unreachable while voya's schema carries no (pxf.default)
		// annotations — defaults live in base.pxf — but the trace can say
		// it, so the message can too.
		return "from a schema default"
	}
	path, ok := layerPaths(dir)[entry.LayerID]
	if !ok {
		return fmt.Sprintf("set by layer %q", entry.LayerID)
	}
	return fmt.Sprintf("set by layer %q (%s)", entry.LayerID, path)
}
