// Package buildinfo answers "which voya is this" from what the Go toolchain
// stamped into the binary. One definition, read by `voya version` and by
// DiagService's Self.version (#149), so the two cannot disagree about which
// build is running.
package buildinfo

import "runtime/debug"

// Version reports the module version go install stamped into the binary, or
// the VCS revision for a local `go build`. Both are absent under `go run`,
// which reads "(devel)".
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	return versionOf(info)
}

// versionOf is [Version] over a build info a test can construct.
func versionOf(info *debug.BuildInfo) string {
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if revision == "" {
		return "(devel)"
	}
	return "(devel) " + revision + modified
}
