package version

import "testing"

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
	if v := from(nil, "", "", ""); v.Commit != "" || v.Time != nil || v.String() != "unknown" {
		t.Errorf("none: %+v", v)
	}
}
