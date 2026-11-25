package boxd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func TestHistoryRangeAndPrune(t *testing.T) {
	dir := t.TempDir()
	now := t0
	h, err := OpenHistory(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	exit0, exit1 := 0, 1
	end := func(d time.Duration) *time.Time { e := t0.Add(d); return &e }
	jobs := []dash.Job{
		{Key: "a/1/x", ID: "1", Box: "a", State: dash.JobDone, Exit: &exit0, StartedAt: t0.Add(-2 * time.Hour), EndedAt: end(-110 * time.Minute), Repo: "https://x/acme/widget.git", Kind: "go-test", Agent: dash.Agent{Session: "s1"}, Tail: "pass"},
		{Key: "a/2/x", ID: "2", Box: "a", State: dash.JobDone, Exit: &exit1, StartedAt: t0.Add(-90 * time.Minute), EndedAt: end(-80 * time.Minute), Repo: "https://x/acme/widget.git", Kind: "go-test", Agent: dash.Agent{Session: "s2"}},
		{Key: "a/3/x", ID: "3", Box: "a", State: dash.JobDone, Exit: &exit0, StartedAt: t0.Add(-40 * 24 * time.Hour), EndedAt: end(-40*24*time.Hour + time.Minute), Tail: "old tail"},
	}
	for _, j := range jobs {
		js := j
		js.State, js.Exit, js.EndedAt = dash.JobRunning, nil, nil
		h.Record(record{Type: recJobStart, At: j.StartedAt, Job: &js})
		h.Record(record{Type: recJobEnd, At: *j.EndedAt, Job: &j})
	}
	h.Record(record{Type: recBoxCreate, At: t0.Add(-3 * time.Hour), Box: &boxRec{Name: "a", Size: "medium", CreatedAt: t0.Add(-3 * time.Hour)}})
	h.Record(record{Type: recMode, At: t0.Add(-30 * time.Hour), Mode: &modeRec{Mode: "full"}})
	h.Record(record{Type: recMode, At: t0.Add(-time.Hour), Mode: &modeRec{Mode: "mypc", Reason: "gaming: x"}})
	h.Samples([]sample{{At: t0.Add(-30 * time.Minute), Kind: "machine", MemUsed: 4 << 30, MemCap: 24 << 30}})

	r := h.Range(dash.RangeDay, Live{Mode: modeRec{Mode: "mypc", Reason: "gaming: x"}, MemCap: 12 << 30})
	b := r.Buckets[21] // 16:00, both jobs
	if b.Jobs.ByResult["pass"] != 1 || b.Jobs.ByResult["fail"] != 1 || b.Jobs.ByRepo["widget"] != 2 || b.MedianSeconds.ByRepo["widget"] != 600 {
		t.Fatalf("16:00 bucket: %+v", b)
	}
	if r.Buckets[22].BoxHours.BySize["medium"] != 1 || r.Buckets[22].MemUsed != 4<<30 ||
		r.Buckets[22].MemCap != 24<<30 || r.Buckets[0].MemCap != 12<<30 {
		t.Fatalf("buckets: %+v %+v", r.Buckets[0], r.Buckets[22])
	}
	if len(r.Segments) != 2 || r.Segments[0].Mode != "full" || !r.Segments[0].Start.Equal(r.Buckets[0].Start) ||
		r.Segments[1].Reason != "gaming: x" || !r.Segments[1].End.Equal(now) {
		t.Fatalf("segments: %+v", r.Segments)
	}
	if p := h.Finished(Filter{Repo: "widget", Limit: 1}); len(p.Jobs) != 1 || p.Jobs[0].ID != "2" || !p.More {
		t.Fatalf("page: %+v", p)
	}
	if p := h.Finished(Filter{Q: "", Result: "fail"}); len(p.Jobs) != 1 {
		t.Fatalf("fail filter: %+v", p)
	}
	first := h.Finished(Filter{Limit: 1}).Jobs[0]
	if p := h.Finished(Filter{Before: &first, Limit: 5}); len(p.Jobs) != 2 || p.Jobs[0].ID != "1" || p.More {
		t.Fatalf("second page: %+v", p)
	}

	// Pruning drops the 40-day-old tail from its file, and a reopened
	// history skips a truncated last line.
	if err := h.Prune(); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "events-"+t0.Add(-40*24*time.Hour).Format("2006-01-02")+".jsonl")
	if b, _ := os.ReadFile(old); strings.Contains(string(b), "old tail") || len(b) == 0 {
		t.Fatalf("old file: %s", b)
	}
	f, _ := os.OpenFile(filepath.Join(dir, "events-"+t0.Format("2006-01-02")+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"v":1,"type":"job-start","job":{"key":"trunc`)
	f.Close()
	h2, _ := OpenHistory(dir, func() time.Time { return now })
	if j, ok := h2.Job("a/1/x"); !ok || j.Tail != "pass" {
		t.Fatalf("reopened: %+v", j)
	}
	if _, ok := h2.Job("trunc"); ok {
		t.Fatal("read a truncated line")
	}
	now = t0.Add(200 * 24 * time.Hour)
	h2.Prune()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("180-day file kept")
	}
}
