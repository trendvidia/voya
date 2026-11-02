package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// writeUserLayer puts a voya.pxf in a fresh directory and returns both.
func writeUserLayer(t *testing.T, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, UserFile)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}
	return dir, path
}

// TestDefaultsWithNoUserLayer is the zero-setup case the whole design is
// for: nothing on disk, and voya still has a configuration it can run with.
// The port values are ADR 001's, and they are fleet convention rather than
// preference — a service's fly.toml declares 9777, so changing this changes
// production too.
func TestDefaultsWithNoUserLayer(t *testing.T) {
	cfg, err := Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.GetProxyPort(), uint32(9777); got != want {
		t.Errorf("proxy_port = %d, want %d", got, want)
	}
	if got, want := cfg.GetDnsPort(), uint32(9778); got != want {
		t.Errorf("dns_port = %d, want %d", got, want)
	}
	if got, want := cfg.GetRegistrationPort(), uint32(9779); got != want {
		t.Errorf("registration_port = %d, want %d", got, want)
	}
	if got, want := cfg.GetLogLevel(), configv1.LogLevel_LOG_LEVEL_INFO; got != want {
		t.Errorf("log_level = %v, want %v", got, want)
	}
	if got, want := cfg.GetShutdownGrace().AsDuration(), 250*time.Millisecond; got != want {
		t.Errorf("shutdown_grace = %v, want %v", got, want)
	}
	if !cfg.GetRequestLog() {
		t.Error("request_log = false by default; ADR 005 has the request log on by default (#150)")
	}
	// ADR 003, amendment of 2026-11-01: only local origins are allowed until a
	// developer names another, so a default entry here is a hole in that.
	if got := cfg.GetCorsOrigins(); len(got) != 0 {
		t.Errorf("cors_origins = %v by default; only a developer's voya.pxf may allow a non-local origin", got)
	}

	// ADR 006's identity provider works with no configuration at all: an
	// issuer, an audience, a lifetime, and a case-named persona set that
	// makes the failure paths reachable (#208).
	auth := cfg.GetAuth()
	if auth == nil {
		t.Fatal("auth = nil by default")
	}
	if auth.GetIssuer() == "" || auth.GetAudience() == "" {
		t.Errorf("auth.issuer = %q, auth.audience = %q; both must be set by base.pxf", auth.GetIssuer(), auth.GetAudience())
	}
	if got := auth.GetTokenLifetime().AsDuration(); got <= 0 {
		t.Errorf("auth.token_lifetime = %v, want positive", got)
	}
	names := make(map[string]*configv1.Persona)
	for _, p := range auth.GetPersonas() {
		names[p.GetName()] = p
	}
	for _, want := range []string{"has-write-scope", "has-read-scope", "no-scopes", "wrong-audience"} {
		if _, ok := names[want]; !ok {
			t.Errorf("base.pxf ships no persona %q; got %v", want, slices.Sorted(maps.Keys(names)))
		}
	}
	if p := names["wrong-audience"]; p != nil && (p.GetAudience() == "" || p.GetAudience() == auth.GetAudience()) {
		t.Errorf("wrong-audience persona has audience %q, which does not differ from auth.audience %q", p.GetAudience(), auth.GetAudience())
	}
	if p := names["no-scopes"]; p != nil && len(p.GetScopes()) != 0 {
		t.Errorf("no-scopes persona has scopes %v", p.GetScopes())
	}
	if p := names["has-write-scope"]; p != nil && !slices.Contains(p.GetScopes(), "write") {
		t.Errorf("has-write-scope persona has scopes %v, no \"write\"", p.GetScopes())
	}
	if p := names["has-read-scope"]; p != nil && (!slices.Contains(p.GetScopes(), "read") || slices.Contains(p.GetScopes(), "write")) {
		t.Errorf("has-read-scope persona has scopes %v, want \"read\" and no \"write\"", p.GetScopes())
	}
}

// TestUserLayerMergesIntoAuth pins the two merge rules the base layer's
// comments promise (#208): a voya.pxf that sets one field of `auth` keeps
// the base layer's personas, because chameleon merges message fields deep,
// and a voya.pxf that sets `personas` replaces the whole list, because it
// merges repeated fields by replacement. An empty list replaces too, which
// made it a chameleon panic before v0.27.1.
func TestUserLayerMergesIntoAuth(t *testing.T) {
	base, err := Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load(base): %v", err)
	}
	if len(base.GetAuth().GetPersonas()) < 2 {
		t.Fatalf("base.pxf ships %d personas; this test needs at least two to tell replacement from a merge", len(base.GetAuth().GetPersonas()))
	}

	t.Run("one field keeps the personas", func(t *testing.T) {
		dir, _ := writeUserLayer(t, "auth { issuer = \"http://idp.test\" }\n")
		cfg, err := Load(context.Background(), dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := cfg.GetAuth().GetIssuer(), "http://idp.test"; got != want {
			t.Errorf("auth.issuer = %q, want %q", got, want)
		}
		if got, want := len(cfg.GetAuth().GetPersonas()), len(base.GetAuth().GetPersonas()); got != want {
			t.Errorf("%d personas after overriding auth.issuer, want the base layer's %d", got, want)
		}
		if got, want := cfg.GetAuth().GetAudience(), base.GetAuth().GetAudience(); got != want {
			t.Errorf("auth.audience = %q, want the base layer's %q", got, want)
		}
	})

	t.Run("personas replace the list", func(t *testing.T) {
		dir, _ := writeUserLayer(t, "auth { personas = [ { name = \"only-this-one\" subject = \"u1\" } ] }\n")
		cfg, err := Load(context.Background(), dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got := cfg.GetAuth().GetPersonas()
		if len(got) != 1 || got[0].GetName() != "only-this-one" {
			t.Errorf("personas = %v, want exactly [only-this-one]: a user layer's list must replace, not append", got)
		}
	})

	t.Run("an empty list removes them all", func(t *testing.T) {
		dir, _ := writeUserLayer(t, "auth { personas = [] }\n")
		cfg, err := Load(context.Background(), dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.GetAuth().GetPersonas(); len(got) != 0 {
			t.Errorf("personas = %v, want none: an empty list is a set value and must replace the base layer's", got)
		}
		if got, want := cfg.GetAuth().GetIssuer(), base.GetAuth().GetIssuer(); got != want {
			t.Errorf("auth.issuer = %q, want the base layer's %q", got, want)
		}
	})
}

// TestMissingConfigDirectory covers the same acceptance criterion one step
// further out: the directory is named but does not exist. A dev tool must
// not treat "you have never configured me" as a failure.
func TestMissingConfigDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")

	cfg, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load with absent directory: %v", err)
	}
	if got, want := cfg.GetProxyPort(), uint32(9777); got != want {
		t.Errorf("proxy_port = %d, want %d", got, want)
	}
}

// TestEmptyUserLayer: the file exists but says nothing. Every field still
// falls through to base.
func TestEmptyUserLayer(t *testing.T) {
	dir, _ := writeUserLayer(t, "# nothing to see here\n")

	cfg, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.GetProxyPort(), uint32(9777); got != want {
		t.Errorf("proxy_port = %d, want %d", got, want)
	}
	if got, want := cfg.GetShutdownGrace().AsDuration(), 250*time.Millisecond; got != want {
		t.Errorf("shutdown_grace = %v, want %v", got, want)
	}
}

// TestUserLayerOverrides checks the merge direction and that a partial file
// overrides only what it names — including for the WKT duration, which
// merges atomically rather than field-by-field.
func TestUserLayerOverrides(t *testing.T) {
	dir, _ := writeUserLayer(t, `
proxy_port     = 19777
log_level      = LOG_LEVEL_DEBUG
shutdown_grace = 2s
request_log    = false
`)

	cfg, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.GetProxyPort(), uint32(19777); got != want {
		t.Errorf("proxy_port = %d, want %d", got, want)
	}
	if got, want := cfg.GetLogLevel(), configv1.LogLevel_LOG_LEVEL_DEBUG; got != want {
		t.Errorf("log_level = %v, want %v", got, want)
	}
	if got, want := cfg.GetShutdownGrace().AsDuration(), 2*time.Second; got != want {
		t.Errorf("shutdown_grace = %v, want %v", got, want)
	}
	// A bool set to its proto zero is still an override: false must win over
	// base's true, and not read as "the user layer said nothing" (#150).
	if cfg.GetRequestLog() {
		t.Error("request_log = true; the user layer's `request_log = false` did not win over base's true")
	}
	// Untouched by the user layer, so still base's.
	if got, want := cfg.GetDnsPort(), uint32(9778); got != want {
		t.Errorf("dns_port = %d, want %d", got, want)
	}
}

// TestBaseLayerSetsEveryField is the guard on the design decision that the
// committed base layer is the only place default values are written down. A
// field added to the schema and not to base.pxf would otherwise resolve to
// its proto zero and be discovered as a listener on port 0.
func TestBaseLayerSetsEveryField(t *testing.T) {
	res, err := resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer res.Destroy()

	if res.Trace == nil {
		t.Fatal("no trace: resolver.WithTrace is what makes error messages name the layer")
	}
	// Singular message fields of voya's own (`auth`) are walked into, so a
	// field added to Auth and not to base.pxf is caught the same way (#208).
	// Well-known types (a Duration) are one value, not a message to walk;
	// elements of a repeated message are values too, and whether the list
	// has the members it should is TestDefaultsWithNoUserLayer's.
	//
	// The trace keys a list element's fields by index, so a persona's
	// `auth.personas[3].audience` cannot stand in for Auth's `auth.audience`.
	var walk func(prefix string, msg protoreflect.Message)
	walk = func(prefix string, msg protoreflect.Message) {
		fields := msg.Descriptor().Fields()
		for i := range fields.Len() {
			fd := fields.Get(i)
			name := prefix + string(fd.Name())
			entry, ok := res.Trace.Entries[name]
			if !ok {
				t.Errorf("%s is not set by any layer — add it to internal/config/layers/base.pxf", name)
				continue
			}
			if entry.LayerID != BaseLayer {
				t.Errorf("%s came from layer %q, want %q", name, entry.LayerID, BaseLayer)
			}
			if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() &&
				strings.HasPrefix(string(fd.Message().FullName()), "voya.") {
				walk(name+".", msg.Get(fd).Message())
			}
		}
	}
	walk("", res.Config.ProtoReflect())
}

// TestPortConflictNamesFieldsAndLayers is the ADR 001 edge: a collision
// between voya's own ports has to be caught before anything binds, and the
// message has to be actionable. "Which layer and field" is the acceptance
// criterion; the file path is what makes it actionable, since a developer
// cannot open a layer id.
func TestPortConflictNamesFieldsAndLayers(t *testing.T) {
	dir, path := writeUserLayer(t, "dns_port = 9777\n")

	_, err := Load(context.Background(), dir)
	if err == nil {
		t.Fatal("Load succeeded with two listeners on port 9777")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("errors.Is(err, ErrInvalid) = false; err = %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"proxy_port", "dns_port", "9777", `layer "user"`, `layer "base"`, path} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

// TestPortWithoutUsableValue covers a port set to zero, which only validation
// stands between and a listener on a random ephemeral port, and a port set to
// null, which the parser refuses on a scalar field since protowire-go v1.13.0.
// Both must fail and name the field; only the first reaches voya's validation,
// so only it is ErrInvalid.
func TestPortWithoutUsableValue(t *testing.T) {
	for _, tc := range []struct {
		body    string
		invalid bool
	}{
		{"proxy_port = 0\n", true},
		{"proxy_port = null\n", false},
	} {
		t.Run(strings.TrimSpace(tc.body), func(t *testing.T) {
			dir, _ := writeUserLayer(t, tc.body)

			_, err := Load(context.Background(), dir)
			if err == nil {
				t.Fatal("Load succeeded")
			}
			if got := errors.Is(err, ErrInvalid); got != tc.invalid {
				t.Errorf("errors.Is(err, ErrInvalid) = %v, want %v; err = %v", got, tc.invalid, err)
			}
			if msg := err.Error(); !strings.Contains(msg, "proxy_port") {
				t.Errorf("error does not name the field:\n%s", msg)
			}
		})
	}
}

// TestMalformedUserLayer: resolution failures must say which layer and
// where, not "invalid config".
func TestMalformedUserLayer(t *testing.T) {
	dir, path := writeUserLayer(t, "proxy_port = \n")

	_, err := Load(context.Background(), dir)
	if err == nil {
		t.Fatal("Load succeeded on a malformed layer")
	}
	var re *ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("error is %T, want *ResolveError: %v", err, err)
	}
	msg := err.Error()
	for _, want := range []string{`"user"`, path} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

// TestUnknownFieldInUserLayer: a typo'd field name is the most likely
// authoring mistake, and silently ignoring it would leave the developer
// wondering why their setting did nothing.
func TestUnknownFieldInUserLayer(t *testing.T) {
	dir, _ := writeUserLayer(t, "proxy_prot = 19777\n")

	_, err := Load(context.Background(), dir)
	if err == nil {
		t.Fatal("Load accepted an unknown field")
	}
	if msg := err.Error(); !strings.Contains(msg, "proxy_prot") {
		t.Errorf("error does not name the offending field:\n%s", msg)
	}
}

// TestUserLayerCannotShadowBase: base.pxf is read from the embedded FS, so
// a file of the same name in the config directory is inert. Worth pinning —
// the two layers are distinguished only by which source they are bound to.
func TestUserLayerCannotShadowBase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base.pxf"), []byte("proxy_port = 1\n"), 0o600); err != nil {
		t.Fatalf("write base.pxf: %v", err)
	}

	cfg, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.GetProxyPort(), uint32(9777); got != want {
		t.Errorf("proxy_port = %d, want %d — a base.pxf in the config directory was read", got, want)
	}
}

func TestValidate(t *testing.T) {
	valid := func() *configv1.Config {
		return &configv1.Config{
			ProxyPort:        9777,
			DnsPort:          9778,
			RegistrationPort: 9779,
			LogLevel:         configv1.LogLevel_LOG_LEVEL_INFO,
			ShutdownGrace:    durationpb.New(250 * time.Millisecond),
			Auth:             validAuth(),
			RateLimit:        validRateLimit(),
		}
	}

	if err := Validate(valid()); err != nil {
		t.Fatalf("Validate(valid) = %v", err)
	}
	persona := func(c *configv1.Config, i int) *configv1.Persona { return c.GetAuth().GetPersonas()[i] }

	cases := []struct {
		name    string
		mutate  func(*configv1.Config)
		mention string
	}{
		{"zero port", func(c *configv1.Config) { c.DnsPort = 0 }, "dns_port"},
		{"port above range", func(c *configv1.Config) { c.DnsPort = 65536 }, "65536"},
		{"duplicate ports", func(c *configv1.Config) { c.RegistrationPort = c.DnsPort }, "registration_port"},
		{"unspecified log level", func(c *configv1.Config) {
			c.LogLevel = configv1.LogLevel_LOG_LEVEL_UNSPECIFIED
		}, "log_level"},
		{"unknown log level", func(c *configv1.Config) { c.LogLevel = configv1.LogLevel(99) }, "99"},
		{"missing shutdown grace", func(c *configv1.Config) { c.ShutdownGrace = nil }, "shutdown_grace"},
		{"negative shutdown grace", func(c *configv1.Config) {
			c.ShutdownGrace = durationpb.New(-time.Second)
		}, "negative"},
		// ADR 006's identity provider (#208): the shapes that would silently
		// mint useless tokens.
		{"missing auth", func(c *configv1.Config) { c.Auth = nil }, "auth is not set"},
		{"empty issuer", func(c *configv1.Config) { c.Auth.Issuer = "" }, "auth.issuer"},
		{"empty audience", func(c *configv1.Config) { c.Auth.Audience = "" }, "auth.audience"},
		{"missing token lifetime", func(c *configv1.Config) { c.Auth.TokenLifetime = nil }, "auth.token_lifetime"},
		{"zero token lifetime", func(c *configv1.Config) { c.Auth.TokenLifetime = durationpb.New(0) }, "auth.token_lifetime"},
		{"negative token lifetime", func(c *configv1.Config) {
			c.Auth.TokenLifetime = durationpb.New(-time.Minute)
		}, "auth.token_lifetime"},
		{"persona without a name", func(c *configv1.Config) { persona(c, 1).Name = "" }, "auth.personas[1] has no name"},
		{"two personas with one name", func(c *configv1.Config) {
			persona(c, 1).Name = persona(c, 0).Name
		}, "auth.personas[0] and auth.personas[1] are both named \"has-write-scope\""},
		{"persona without a subject", func(c *configv1.Config) { persona(c, 1).Subject = "" }, "has no subject"},
		{"empty scope", func(c *configv1.Config) { persona(c, 0).Scopes = []string{"read", ""} }, "space-joined"},
		{"scope with whitespace", func(c *configv1.Config) {
			persona(c, 0).Scopes = []string{"read write"}
		}, "\"read write\""},
		{"persona restates a stamped claim", func(c *configv1.Config) {
			persona(c, 0).Claims = map[string]string{"tenant": "acme", "exp": "0"}
		}, "\"exp\""},
		{"claim with an empty name", func(c *configv1.Config) {
			persona(c, 0).Claims = map[string]string{"": "acme"}
		}, "empty name"},
		// The local rate limiter (#223): the shapes an enabled one cannot
		// run with.
		{"missing rate limit", func(c *configv1.Config) { c.RateLimit = nil }, "rate_limit is not set"},
		{"cors origin with a wildcard", func(c *configv1.Config) { c.CorsOrigins = []string{"https://*.example.test"} }, "wildcards"},
		{"cors origin with a path", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test/"} }, "a path"},
		{"cors origin with no scheme", func(c *configv1.Config) { c.CorsOrigins = []string{"example.test"} }, "must be http or https"},
		{"cors origin that is null", func(c *configv1.Config) { c.CorsOrigins = []string{"null"} }, "must be http or https"},
		{"cors origin with another scheme", func(c *configv1.Config) { c.CorsOrigins = []string{"chrome-extension://abc"} }, "must be http or https"},
		{"cors origin with no host", func(c *configv1.Config) { c.CorsOrigins = []string{"https://"} }, "no host"},
		{"cors origin with a user name", func(c *configv1.Config) { c.CorsOrigins = []string{"https://me@example.test"} }, "user name"},
		{"cors origin with a query", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test?x=1"} }, "a query"},
		{"cors origin with a fragment", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test#top"} }, "a fragment"},
		{"cors origin with an upper-case scheme", func(c *configv1.Config) { c.CorsOrigins = []string{"HTTPS://example.test"} }, "lower case"},
		{"cors origin with the default https port", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test:443"} }, "default port"},
		{"cors origin with the default http port", func(c *configv1.Config) { c.CorsOrigins = []string{"http://example.test:80"} }, "default port"},
		{"cors origin with a bare colon", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test:"} }, "no port after it"},
		{"cors origin above the highest port", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test:65536"} }, "never send port"},
		{"cors origin with port zero", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test:0"} }, "never send port"},
		{"cors origin with a padded port", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test:08443"} }, "never send port"},
		{"cors origin with a non-ascii host", func(c *configv1.Config) { c.CorsOrigins = []string{"https://b\u00fccher.test"} }, "punycode"},
		{"cors origin that does not parse", func(c *configv1.Config) { c.CorsOrigins = []string{"https://example.test:abc"} }, "does not parse"},
		{"cors origin with an ipv6 zone", func(c *configv1.Config) { c.CorsOrigins = []string{"http://[fe80::1%25en0]"} }, "write exactly"},
		{"cors origin after a good one", func(c *configv1.Config) {
			c.CorsOrigins = []string{"https://good.example.test", "https://bad.example.test/"}
		}, "bad.example.test"},
		{"enabled with zero requests", func(c *configv1.Config) { c.RateLimit.Requests = 0 }, "rate_limit.requests"},
		{"enabled with no window", func(c *configv1.Config) { c.RateLimit.Window = nil }, "rate_limit.window"},
		{"enabled with zero window", func(c *configv1.Config) {
			c.RateLimit.Window = durationpb.New(0)
		}, "rate_limit.window"},
		{"enabled with negative window", func(c *configv1.Config) {
			c.RateLimit.Window = durationpb.New(-time.Minute)
		}, "rate_limit.window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(c)
			err := Validate(c)
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("errors.Is(err, ErrInvalid) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error does not mention %q:\n%s", tc.mention, err)
			}
			// Validate passes no trace, so a detail line is the bare field, or
			// the bare field and its value. One that leads with anything else
			// went round originLine or detail, and mention cannot see that,
			// because the Problem names the field on its own.
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("err = %v, want a ValidationError", err)
			}
			if len(verr.Details) == 0 {
				t.Errorf("no detail line:\n%s", err)
			}
			for _, d := range verr.Details {
				if f := strings.Fields(d); len(f) == 0 || !isConfigField(f[0]) {
					t.Errorf("detail line %q does not lead with a config field:\n%s", d, err)
				}
			}
		})
	}

	if err := Validate(nil); err == nil {
		t.Error("Validate(nil) = nil")
	}
}

// isConfigField reports whether path, such as auth.personas, walks the schema
// from Config.
func isConfigField(path string) bool {
	md := (&configv1.Config{}).ProtoReflect().Descriptor()
	for part := range strings.SplitSeq(path, ".") {
		if md == nil {
			return false
		}
		fd := md.Fields().ByName(protoreflect.Name(part))
		if fd == nil {
			return false
		}
		md = fd.Message()
	}
	return true
}

// validAuth is an Auth that validates, with two named personas so a test
// can make them collide. Unrelated claims and a per-persona audience are
// set on purpose: those are the shapes validation must leave alone.
func validAuth() *configv1.Auth {
	return &configv1.Auth{
		Issuer:        "http://voya.internal:9777",
		Audience:      "internal",
		TokenLifetime: durationpb.New(time.Hour),
		Personas: []*configv1.Persona{
			{Name: "has-write-scope", Subject: "u1", Scopes: []string{"read", "write"}, Claims: map[string]string{"tenant": "acme"}},
			{Name: "wrong-audience", Subject: "u2", Audience: "not-this-service"},
		},
	}
}

// validRateLimit is a RateLimit that validates, enabled on purpose: the
// threshold checks apply only to an enabled limiter, so a disabled fixture
// would leave every one of TestValidate's rate_limit cases passing vacuously.
func validRateLimit() *configv1.RateLimit {
	return &configv1.RateLimit{
		Enabled:  true,
		Requests: 5,
		Window:   durationpb.New(time.Minute),
	}
}

// TestValidateAcceptsDisabledRateLimitWithZeroThresholds: a disabled
// limiter's thresholds are inert, so a layer that turned it off without
// restating them must not be refused for values nothing will read.
func TestValidateAcceptsDisabledRateLimitWithZeroThresholds(t *testing.T) {
	c := &configv1.Config{
		ProxyPort:        9777,
		DnsPort:          9778,
		RegistrationPort: 9779,
		LogLevel:         configv1.LogLevel_LOG_LEVEL_INFO,
		ShutdownGrace:    durationpb.New(250 * time.Millisecond),
		Auth:             validAuth(),
		RateLimit:        &configv1.RateLimit{},
	}
	if err := Validate(c); err != nil {
		t.Errorf("Validate = %v", err)
	}
}

// TestValidateAcceptsNoPersonas: an empty persona list is a coherent
// configuration — `voya token` then has nothing to mint, and says so — and
// must not be confused with a broken one.
func TestValidateAcceptsNoPersonas(t *testing.T) {
	c := &configv1.Config{
		ProxyPort:        9777,
		DnsPort:          9778,
		RegistrationPort: 9779,
		LogLevel:         configv1.LogLevel_LOG_LEVEL_INFO,
		ShutdownGrace:    durationpb.New(250 * time.Millisecond),
		Auth:             validAuth(),
		RateLimit:        validRateLimit(),
	}
	c.Auth.Personas = nil
	if err := Validate(c); err != nil {
		t.Errorf("Validate = %v", err)
	}
}

// TestValidateAcceptsZeroGrace: 0 means "do not wait", which is a coherent
// choice for a fast restart loop and must not be confused with "unset".
func TestValidateAcceptsZeroGrace(t *testing.T) {
	c := &configv1.Config{
		ProxyPort:        9777,
		DnsPort:          9778,
		RegistrationPort: 9779,
		LogLevel:         configv1.LogLevel_LOG_LEVEL_INFO,
		ShutdownGrace:    durationpb.New(0),
		Auth:             validAuth(),
		RateLimit:        validRateLimit(),
	}
	if err := Validate(c); err != nil {
		t.Errorf("Validate = %v", err)
	}
}

func TestDir(t *testing.T) {
	t.Run("VOYA_CONFIG_DIR wins", func(t *testing.T) {
		t.Setenv(DirEnv, "/tmp/voya-test")
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
		if got, want := Dir(), "/tmp/voya-test"; got != want {
			t.Errorf("Dir() = %q, want %q", got, want)
		}
	})

	t.Run("XDG_CONFIG_HOME next", func(t *testing.T) {
		t.Setenv(DirEnv, "")
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
		if got, want := Dir(), filepath.Join("/tmp/xdg", "voya"); got != want {
			t.Errorf("Dir() = %q, want %q", got, want)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("~/.config is the Unix convention; Windows uses os.UserConfigDir")
		}
		t.Setenv(DirEnv, "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/tmp/home")
		if got, want := Dir(), filepath.Join("/tmp/home", ".config", "voya"); got != want {
			t.Errorf("Dir() = %q, want %q", got, want)
		}
	})
}

func TestAddr(t *testing.T) {
	if got, want := Addr(9777), "127.0.0.1:9777"; got != want {
		t.Errorf("Addr(9777) = %q, want %q", got, want)
	}
}

// TestEmptyCORSOriginsInAUserLayer loads the `cors_origins = []` that panicked
// in chameleon before v0.27.1. base.pxf writes the same empty list, so the
// value cannot show the user layer was read, and the layer it is credited to
// can, and TestBaseLayerSetsEveryField holds it to base without one.
// TestUserLayerMergesIntoAuth has the case where an empty list changes a value.
func TestEmptyCORSOriginsInAUserLayer(t *testing.T) {
	dir, _ := writeUserLayer(t, "cors_origins = []\n")
	cfg, layer, err := LoadOrigin(context.Background(), dir, "cors_origins")
	if err != nil {
		t.Fatal(err)
	}
	if layer != UserLayer {
		t.Errorf("cors_origins comes from layer %q, want %q", layer, UserLayer)
	}
	if got := cfg.GetCorsOrigins(); len(got) != 0 {
		t.Errorf("cors_origins = %v, want none", got)
	}
}

// TestCORSOriginErrorNamesTheField holds the detail line to what originLine
// renders through Load, where a bare origin call left "set by layer" with no
// field in front of it. TestValidate has the path with no trace. The file is in
// the expected line because a developer cannot open a layer id.
func TestCORSOriginErrorNamesTheField(t *testing.T) {
	dir, path := writeUserLayer(t, "cors_origins = [\"example.test\"]\n")
	_, err := Load(context.Background(), dir)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Load err = %v, want a ValidationError", err)
	}
	want := fmt.Sprintf("cors_origins set by layer %q (%s)", UserLayer, path)
	if got := verr.Details; len(got) != 1 || got[0] != want {
		t.Errorf("through Load, Details = %q, want [%s]", got, want)
	}
}

// TestCORSOriginWithADefaultPortFailsLoad takes the entry a developer is most
// likely to write through a voya.pxf: it looks right, and with no refusal it
// would load and then match nothing a browser sends.
func TestCORSOriginWithADefaultPortFailsLoad(t *testing.T) {
	dir, _ := writeUserLayer(t, "cors_origins = [\"https://example.test:443\"]\n")
	_, err := Load(context.Background(), dir)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Load err = %v, want a ValidationError", err)
	}
	if !strings.Contains(verr.Problem, "default port") {
		t.Errorf("Problem = %q, want it to name the default port", verr.Problem)
	}
}

// TestCORSOriginsABrowserSends is what keeps the refusals in TestValidate from
// being satisfied by refusing everything. Each entry is a value a browser sends
// in Origin, including the ports that are a default only for the other scheme.
func TestCORSOriginsABrowserSends(t *testing.T) {
	for _, entry := range []string{
		"https://app.example.test",
		"https://app.example.test:8443",
		"http://app.example.test:443",
		"https://app.example.test:80",
		"https://app.example.test:65535",
		"https://xn--bcher-kva.test",
		"http://192.0.2.1:3000",
		"http://[2001:db8::1]",
		"http://[2001:db8::1]:8080",
	} {
		if err := validateCORSOrigins([]string{entry}, func(string) string { return "" }); err != nil {
			t.Errorf("%s: %v", entry, err)
		}
	}
}

// TestCORSOriginsResolveFromAUserLayer is the path a developer takes.
func TestCORSOriginsResolveFromAUserLayer(t *testing.T) {
	dir, _ := writeUserLayer(t, "cors_origins = [\"https://app.example.test\"]\n")
	cfg, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetCorsOrigins(); len(got) != 1 || got[0] != "https://app.example.test" {
		t.Errorf("cors_origins = %v", got)
	}
}
