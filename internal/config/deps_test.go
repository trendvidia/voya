package config

import (
	"os"
	"strings"
	"testing"

	"github.com/trendvidia/chameleon/chain"
	chameleonv1 "github.com/trendvidia/chameleon/proto/chameleon/v1"
)

// This package is where chameleon enters voya, and for one release it brought
// every cloud KMS SDK with it (#28). chameleon's resolver imported
// chameleon/sops at package level, so a consumer resolving two plaintext .pxf
// layers with no decrypter registered still linked AWS, Azure, GCP, Vault and
// Huawei key management: 66 MB of binary and 350 modules, against 17 MB and 64
// once chameleon v0.23.0 put decryption behind a registered crypt.Decrypter
// and split sops into its own module.
//
// chameleon pins its own half of that (TestPlaintextPathLinksNoKeyManagement,
// TestLibraryModuleRequiresNoKeyManagement). What those cannot see is voya's
// graph: anything voya requires that pulls chameleon/sops back — a future
// dependency, or a blank import added for an encrypted layer — reinstates the
// whole tree, and the only visible symptom is a go.sum diff nobody reads
// closely and a binary nobody weighs.
//
// The check is a blocklist, so it cannot notice a key-management SDK that
// nobody has heard of yet. It notices the one route that actually regressed.
//
// Seven of these eight match the go.mod this upgrade replaced, and
// TestTheKeyManagementCheckCanFail asserts it against that file rather than
// leaving it as a claim here. The exception is chameleon/sops, which matches
// nothing there and is not a mistake: at v0.22.1 it was a package inside the
// chameleon module and could never appear in a require line. It is the marker
// for the *next* reintroduction rather than the last one, since v0.23.0 made
// it a module of its own — which is exactly what a blank import would now have
// to require.
var keyManagement = []string{
	"github.com/trendvidia/chameleon/sops",
	"github.com/trendvidia/sops/",
	"github.com/aws/aws-sdk-go",
	"github.com/Azure/azure-sdk-for-go",
	"github.com/AzureAD/microsoft-authentication-library-for-go",
	"cloud.google.com/go/kms",
	"github.com/hashicorp/vault/api",
	"github.com/huaweicloud/huaweicloud-sdk-go",
}

// forwardLooking is the one pattern that is expected to match nothing in the
// pre-upgrade go.mod, because the module it names did not exist yet. It is
// checked against sopsModuleRequire instead.
const forwardLooking = "github.com/trendvidia/chameleon/sops"

// sopsModuleRequire is the require line that reintroducing the sops backend
// would add today — a blank import of the module chameleon v0.23.0 split out.
// It is written out in full rather than built from the pattern it checks,
// which is the point: a typo in that pattern has nothing to match here.
const sopsModuleRequire = "\tgithub.com/trendvidia/chameleon/sops v0.23.0 // indirect\n"

// readMod reads a go.mod for the check to run over.
func readMod(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// keyManagementIn reports which blocklist patterns appear in mod. Both tests
// below call this same function — one against the go.mod voya ships, one
// against the go.mod this upgrade replaced — so the negative control exercises
// the real check rather than a restatement of it.
func keyManagementIn(mod string) []string {
	var found []string
	for _, path := range keyManagement {
		if strings.Contains(mod, path) {
			found = append(found, path)
		}
	}
	return found
}

// TestNoKeyManagementInTheModuleGraph fails if voya requires a key-management
// SDK again.
func TestNoKeyManagementInTheModuleGraph(t *testing.T) {
	for _, path := range keyManagementIn(readMod(t, "../../go.mod")) {
		t.Errorf("go.mod requires %s — voya resolves plaintext .pxf layers and needs no "+
			"key management (#28). Something reintroduced chameleon/sops or an equivalent; "+
			"check `go mod why -m %s` before accepting a 50 MB binary.", path, path)
	}
}

// TestTheKeyManagementCheckCanFail is the negative control for the test above.
//
// A blocklist that matches nothing passes whether or not the property holds,
// and a typo in one of these paths would be invisible: the check would go
// quiet and read as "no key management" forever. So the same check is run over
// testdata/go.mod.pre-upgrade — the go.mod this upgrade replaced, copied
// verbatim — where seven of the eight patterns are known to appear. A pattern
// that fires there is one that would fire on a real regression; a pattern that
// does not is a typo, not a property.
//
// The corpus matters more than the assertion. It is a file the patterns did
// not build, which is what the previous version of this test got wrong: it
// searched a string concatenated from the pattern itself and so could never
// fail for any input.
func TestTheKeyManagementCheckCanFail(t *testing.T) {
	if len(keyManagement) == 0 {
		t.Fatal("the blocklist is empty, so the check above passes vacuously")
	}

	fired := make(map[string]bool)
	for _, path := range keyManagementIn(readMod(t, "testdata/go.mod.pre-upgrade")) {
		fired[path] = true
	}

	for _, path := range keyManagement {
		if path == forwardLooking {
			if fired[path] {
				t.Errorf("pattern %q matched the pre-upgrade go.mod, but it is documented as "+
					"matching nothing there — the fixture or the comment above is wrong", path)
			}
			if !strings.Contains(sopsModuleRequire, path) {
				t.Errorf("pattern %q does not match %q, the require line a blank import of the "+
					"sops backend would add, so it guards nothing", path, strings.TrimSpace(sopsModuleRequire))
			}
			continue
		}
		if !fired[path] {
			t.Errorf("pattern %q matches nothing in testdata/go.mod.pre-upgrade, which carried "+
				"the whole key-management tree — so it would not have caught the regression it "+
				"is named for. Check it for a typo.", path)
		}
	}
}

// TestNoChainLayerIsMarkedEncrypted pins the precondition the two tests above
// depend on.
//
// Linking no key management is not a property of the dependency graph alone —
// it holds because nothing asks voya to decrypt. chameleon gates decryption on
// `encrypted = true` in the chain spec (chain.LayerEntry.encrypted), not on
// anything about the file at the other end, so with both of voya's layers
// unmarked the resolver never reaches for a Decrypter and none has to be
// registered.
//
// That also bounds the release note: because the gate is the spec and not the
// file, encrypting voya.pxf is a parse failure both before and after v0.23.0,
// not the `encrypted layer without Decrypter` the first draft of the fragment
// claimed. Adding `encrypted = true` to layers/chain.pxf is the change that
// would need a backend registered — which is a blank import of
// chameleon/sops, and the whole key-management tree with it. Fail here rather
// than at run time on a developer's laptop.
func TestNoChainLayerIsMarkedEncrypted(t *testing.T) {
	specData, err := embedded.ReadFile("layers/chain.pxf")
	if err != nil {
		t.Fatalf("reading the embedded chain spec: %v", err)
	}
	msg, err := chain.ParseSpec(specData)
	if err != nil {
		t.Fatalf("parsing the embedded chain spec: %v", err)
	}
	spec, ok := msg.(*chameleonv1.ChainSpec)
	if !ok {
		t.Fatalf("chain.ParseSpec returned %T, want *chameleonv1.ChainSpec", msg)
	}

	if len(spec.GetLayers()) == 0 {
		t.Fatal("the chain spec declares no layers, so this test would pass vacuously")
	}
	for _, layer := range spec.GetLayers() {
		if layer.GetEncrypted() {
			t.Errorf("chain layer %q is marked encrypted, which makes chameleon require a "+
				"registered Decrypter — voya registers none, so resolution fails with "+
				"resolver.ErrNoDecrypter. Registering one means blank-importing "+
				"github.com/trendvidia/chameleon/sops and taking the key-management tree "+
				"back (#28); TestNoKeyManagementInTheModuleGraph will fail alongside this.",
				layer.GetId())
		}
	}
}
