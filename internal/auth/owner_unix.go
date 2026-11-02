//go:build unix

package auth

import (
	"io/fs"
	"syscall"
)

// fileOwner answers the uid that owns the file info describes. On unix that
// is the Uid of the stat structure the FileInfo carries; a FileInfo that
// carries something else (a test double, a filesystem Go has no stat for)
// answers "not known", and [checkOwner] then lets the file through rather
// than refusing a key over a question it could not ask.
func fileOwner(info fs.FileInfo) (uid int, known bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
