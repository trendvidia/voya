package diag

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

func TestInstanceWordsCarriesThePidOnlyWhenThereIsOne(t *testing.T) {
	if got, want := InstanceWords(&diagv1.Instance{Port: 4100}), "port 4100"; got != want {
		t.Errorf("no pid: got %q, want %q", got, want)
	}
	if got, want := InstanceWords(&diagv1.Instance{Port: 4100, Pid: 7}), "port 4100, pid 7"; got != want {
		t.Errorf("with a pid: got %q, want %q", got, want)
	}
}

func TestInstantRendersLocalTimeOrADash(t *testing.T) {
	if got := Instant(nil); got != "-" {
		t.Errorf("unset: got %q, want -", got)
	}
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got, want := Instant(timestamppb.New(at)), at.Local().Format(TimeFormat); got != want {
		t.Errorf("set: got %q, want %q", got, want)
	}
}
