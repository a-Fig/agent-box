package dash

import (
	"net/url"
	"testing"
	"time"
)

func TestRanOn(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	first := Box{Name: "widget", Machine: "pc", UID: "u1", CreatedAt: t0}
	// The replacement, made 3s after the first box's last job started.
	repl := Box{Name: "widget", Machine: "pc", UID: "u2", CreatedAt: t0.Add(2*time.Minute + 3*time.Second)}
	cases := []struct {
		name string
		j    Job
		b    Box
		want bool
	}{
		// box run --new names the job before it launches the box; the laptop's clock may be behind too.
		{"named before its box", Job{Box: "widget", Machine: "pc", BoxUID: "u1", StartedAt: t0.Add(-5 * time.Minute)}, first, true},
		{"the first box's last job, on the replacement", Job{Box: "widget", Machine: "pc", BoxUID: "u1", StartedAt: t0.Add(2 * time.Minute)}, repl, false},
		{"the replacement's job, on the first box", Job{Box: "widget", Machine: "pc", BoxUID: "u2", StartedAt: t0.Add(2 * time.Minute)}, first, false},
		{"another name", Job{Box: "gadget", Machine: "pc", BoxUID: "u1", StartedAt: t0}, first, false},
		{"another machine", Job{Box: "widget", Machine: "pc2", BoxUID: "u1", StartedAt: t0}, first, false},
		// Jobs recorded before BoxUID: the time rule.
		{"legacy, within the skew", Job{Box: "widget", Machine: "pc", StartedAt: t0.Add(-BoxJobSkew)}, first, true},
		{"legacy, before the skew", Job{Box: "widget", Machine: "pc", StartedAt: t0.Add(-BoxJobSkew - time.Second)}, first, false},
	}
	for _, c := range cases {
		if got := c.j.RanOn(c.b); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestParseBoxQuery(t *testing.T) {
	ok := url.Values{"box": {"widget"}, "machine": {"pc"}, "uid": {"u1"}, "created": {"2026-11-25T12:00:00Z"}}
	if b, err := ParseBoxQuery(ok); err != nil || b.UID != "u1" || b.CreatedAt.Hour() != 12 {
		t.Fatalf("%+v %v", b, err)
	}
	for _, drop := range []string{"box", "machine", "uid", "created"} {
		q := url.Values{}
		for k, v := range ok {
			if k != drop {
				q[k] = v
			}
		}
		if _, err := ParseBoxQuery(q); err == nil {
			t.Errorf("no %s: accepted", drop)
		}
	}
}
