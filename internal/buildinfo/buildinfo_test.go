package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestVersionOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{"go install stamps the module version", debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, "v1.2.3"},
		{"a local build reads the revision", debug.BuildInfo{
			Main:     debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "false"}},
		}, "(devel) abc123"},
		{"a dirty tree says so", debug.BuildInfo{
			Main:     debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}},
		}, "(devel) abc123-dirty"},
		{"go run has neither", debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "(devel)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionOf(&tc.info); got != tc.want {
				t.Errorf("versionOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVersionSaysSomething: whatever this binary is, the answer is never
// empty — Self.version and `voya version` both print it.
func TestVersionSaysSomething(t *testing.T) {
	if Version() == "" {
		t.Error("Version() = \"\"")
	}
}
