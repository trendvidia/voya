package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
)

// TestKeySetIsAnRFC7517SetWithTheHeadersAClientSelectsBy: the document
// parses with a stock JOSE library, holds exactly the one signing key, and
// carries kid, alg and use — the three members a validator filters on.
func TestKeySetIsAnRFC7517SetWithTheHeadersAClientSelectsBy(t *testing.T) {
	key, err := LoadOrGenerate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	doc, err := KeySet(key)
	if err != nil {
		t.Fatalf("KeySet: %v", err)
	}
	set, err := jwk.Parse(doc)
	if err != nil {
		t.Fatalf("a stock JOSE library does not parse the document: %v\n%s", err, doc)
	}
	if set.Len() != 1 {
		t.Fatalf("the set holds %d keys, want exactly the signing key", set.Len())
	}
	k, _ := set.Key(0)
	if k.KeyID() == "" {
		t.Error("the key has no kid; a validator cannot match a token's header to it")
	}
	if got := k.Algorithm(); got != jwa.RS256 {
		t.Errorf("alg = %q, want %q", got, jwa.RS256)
	}
	if got := k.KeyUsage(); got != string(jwk.ForSignature) {
		t.Errorf("use = %q, want %q", got, jwk.ForSignature)
	}
	if got := k.KeyType(); got != jwa.RSA {
		t.Errorf("kty = %q, want %q", got, jwa.RSA)
	}
}

// serveJWKS drives the handler for dir with one request and returns the
// recorded response.
func serveJWKS(t *testing.T, dir, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	JWKSHandler(dir).ServeHTTP(rec, httptest.NewRequest(method, JWKSPath, nil))
	return rec
}

func TestJWKSHandlerServesTheKeySet(t *testing.T) {
	dir := t.TempDir()
	res := serveJWKS(t, dir, http.MethodGet)
	if res.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body:\n%s", res.Code, res.Body)
	}
	if got := res.Header().Get("Content-Type"); got != JWKSContentType {
		t.Errorf("content-type = %q, want %q", got, JWKSContentType)
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want %q — a cached copy outliving a regenerated key is a stale-key failure that reads as a bad signature", got, "no-store")
	}
	set, err := jwk.Parse(res.Body.Bytes())
	if err != nil || set.Len() != 1 {
		t.Fatalf("the body is not a one-key JWK Set (err %v):\n%s", err, res.Body)
	}

	// The key it serves is the key on disk: the same document KeySet renders
	// for what LoadOrGenerate now returns, kid for kid.
	key, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("LoadOrGenerate after the handler ran: %v", err)
	}
	want, err := KeySet(key)
	if err != nil {
		t.Fatalf("KeySet: %v", err)
	}
	if res.Body.String() != string(want) {
		t.Errorf("the served set is not the on-disk key's:\nserved %s\nwant   %s", res.Body, want)
	}
}

// TestJWKSHandlerAnswersHEAD: a HEAD gets the GET's status and headers —
// the body is the server's to suppress, not the handler's.
func TestJWKSHandlerAnswersHEAD(t *testing.T) {
	res := serveJWKS(t, t.TempDir(), http.MethodHead)
	if res.Code != http.StatusOK {
		t.Fatalf("HEAD = %d, want 200; body:\n%s", res.Code, res.Body)
	}
	if got := res.Header().Get("Content-Type"); got != JWKSContentType {
		t.Errorf("content-type = %q, want %q", got, JWKSContentType)
	}
}

// TestJWKSHandlerRefusesOtherMethods: a POST to the well-known path is 405
// with Allow — not the reserved row's 501, because the path exists, and not
// a 200, because the document is fetched and never posted to.
func TestJWKSHandlerRefusesOtherMethods(t *testing.T) {
	dir := t.TempDir()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		res := serveJWKS(t, dir, method)
		if res.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, res.Code)
		}
		if got := res.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s Allow = %q, want %q", method, got, "GET, HEAD")
		}
		if strings.Contains(res.Body.String(), `"keys"`) {
			t.Errorf("%s answered with a key set:\n%s", method, res.Body)
		}
	}
}

// TestJWKSHandlerRegeneratesADeletedKey pins the per-request load: the key
// file deleted between requests is regenerated (#208's contract), and the
// served set changes with it rather than answering from a stale copy.
func TestJWKSHandlerRegeneratesADeletedKey(t *testing.T) {
	dir := t.TempDir()
	first := serveJWKS(t, dir, http.MethodGet)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET = %d; body:\n%s", first.Code, first.Body)
	}
	if err := os.Remove(filepath.Join(dir, KeyFile)); err != nil {
		t.Fatalf("deleting the key: %v", err)
	}
	second := serveJWKS(t, dir, http.MethodGet)
	if second.Code != http.StatusOK {
		t.Fatalf("GET after deleting the key = %d, want 200 with a fresh key; body:\n%s", second.Code, second.Body)
	}
	if first.Body.String() == second.Body.String() {
		t.Error("the served set did not change after the key file was deleted — the handler is answering from a copy, not from disk")
	}
}

// TestJWKSHandlerNeverServesAnEmptySet: an unreadable key is a 500 naming
// the trouble — never a 200 with an empty key set, which a JOSE client
// caches as "this issuer has no keys" and then fails closed for its TTL.
func TestJWKSHandlerNeverServesAnEmptySet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFile)
	if err := os.WriteFile(path, []byte("not a key\n"), KeyMode); err != nil {
		t.Fatalf("planting the broken key: %v", err)
	}
	res := serveJWKS(t, dir, http.MethodGet)
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("GET with an unreadable key = %d, want 500; body:\n%s", res.Code, res.Body)
	}
	if strings.Contains(res.Body.String(), `"keys"`) {
		t.Errorf("the failure body reads as a key set:\n%s", res.Body)
	}
	if !strings.Contains(res.Body.String(), path) {
		t.Errorf("the failure does not name the key file, so it names no remedy:\n%s", res.Body)
	}
}
