package diag

import (
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// InstanceWords names one instance the way registry.Eviction's announcement
// does — "port N, pid M", the pid only when the registration carried one — so
// the daemon's warning and the `voya status` record name the same instance in
// the same words.
func InstanceWords(inst *diagv1.Instance) string {
	if inst.GetPid() == 0 {
		return fmt.Sprintf("port %d", inst.GetPort())
	}
	return fmt.Sprintf("port %d, pid %d", inst.GetPort(), inst.GetPid())
}

// TimeFormat is how `voya status` writes an instant: the request log's format
// without the milliseconds, in local time, so a `voya log` line and a status
// timestamp read as one clock.
const TimeFormat = "2006-01-02T15:04:05Z07:00"

// Instant renders a timestamp in [TimeFormat], or "-" for one the daemon did
// not set.
func Instant(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "-"
	}
	return ts.AsTime().Local().Format(TimeFormat)
}
