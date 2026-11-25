package version

import (
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func TestText(t *testing.T) {
	at := time.Date(2026, 11, 25, 7, 12, 59, 0, time.UTC)
	for _, tc := range []struct {
		v    dash.Version
		want string
	}{
		{dash.Version{Commit: "e823842abcdef", Time: &at, Tag: "v0.3.0"}, "box v0.3.0 (e823842, 2026-11-25 07:12 UTC)"},
		{dash.Version{Commit: "e823842abcdef", Tag: "v0.3.1-nightly.20261125.2"}, "box v0.3.1-nightly.20261125.2 (e823842)"},
		{dash.Version{Commit: "1378501abcdef", Time: &at, Modified: true}, "box 1378501+ (2026-11-25 07:12 UTC)"},
		{dash.Version{}, "box unknown"},
	} {
		if got := Text("box", tc.v); got != tc.want {
			t.Errorf("Text(%+v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestFrom(t *testing.T) {
	stamp := map[string]string{"vcs.revision": "1378501abcdef", "vcs.time": "2026-11-24T22:42:59Z", "vcs.modified": "true"}
	v := from(stamp, "", "", "")
	if v.Commit != "1378501abcdef" || v.Time == nil || v.Time.Hour() != 22 || !v.Modified || v.String() != "1378501+" {
		t.Errorf("stamped: %+v %s", v, v)
	}
	// -X wins over the stamp, and the stamp's time doesn't go with another commit.
	v = from(stamp, "e823842ffff", "", "false")
	if v.Commit != "e823842ffff" || v.Time != nil || v.Modified || v.String() != "e823842" {
		t.Errorf("-X: %+v %s", v, v)
	}
	// An offset time comes out in UTC.
	v = from(nil, "abc", "2026-11-24T14:42:59-08:00", "")
	if v.Time == nil || !v.Time.Equal(*from(stamp, "", "", "").Time) || v.Time.Location().String() != "UTC" {
		t.Errorf("offset: %+v", v)
	}
	// Time and Modified don't mix with the stamp's commit.
	if v := from(stamp, "", "2020-01-01T00:00:00Z", "false"); !v.Modified || v.Time.Year() != 2026 {
		t.Errorf("mixed: %+v", v)
	}
	if v := from(nil, "", "", ""); v.Commit != "" || v.Time != nil || v.String() != "unknown" {
		t.Errorf("none: %+v", v)
	}
}
