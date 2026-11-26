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
	h.Record(record{Type: recMode, At: t0.Add(-time.Hour), Mode: &modeRec{Mode: "mypc"}})
	h.Samples([]sample{{At: t0.Add(-30 * time.Minute), Kind: "machine", MemUsed: 4 << 30, MemCap: 24 << 30}})

	r := h.Range(dash.RangeDay, Live{Mode: modeRec{Mode: "mypc"}, MemCap: 12 << 30})
	b := r.Buckets[21] // 16:00, both jobs
	if b.Jobs.ByResult["pass"] != 1 || b.Jobs.ByResult["fail"] != 1 || b.Jobs.ByRepo["widget"] != 2 || b.MedianSeconds.ByRepo["widget"] != 600 {
		t.Fatalf("16:00 bucket: %+v", b)
	}
	if r.Buckets[22].BoxHours.BySize["medium"] != 1 || r.Buckets[22].MemUsed != 4<<30 ||
		r.Buckets[22].MemCap != 24<<30 || r.Buckets[0].MemCap != 12<<30 {
		t.Fatalf("buckets: %+v %+v", r.Buckets[0], r.Buckets[22])
	}
	if len(r.Segments) != 2 || r.Segments[0].Mode != "full" || !r.Segments[0].Start.Equal(r.Buckets[0].Start) ||
		r.Segments[1].Mode != "mypc" || !r.Segments[1].End.Equal(now) {
		t.Fatalf("segments: %+v", r.Segments)
	}
	if p := h.Finished(Filter{Repo: "widget", Limit: 1}); len(p.Jobs) != 1 || p.Jobs[0].ID != "2" || !p.More {
		t.Fatalf("page: %+v", p)
	}
	if p := h.Finished(Filter{Q: "", Result: "fail"}); len(p.Jobs) != 1 {
		t.Fatalf("fail filter: %+v", p)
	}
	if p := h.Finished(Filter{Box: "a", Since: t0.Add(-100 * time.Minute)}); len(p.Jobs) != 1 || p.Jobs[0].ID != "2" {
		t.Fatalf("box since filter: %+v", p)
	}
	if p := h.Finished(Filter{Box: "b"}); len(p.Jobs) != 0 {
		t.Fatalf("box filter: %+v", p)
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

// Queued jobs count in the hour they were queued, cancelled ones too; a
// wait counts in the hour its job started. A reopened history says the same.
func TestHistoryCountsQueuedJobsAndWaits(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHistory(dir, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	j := &dash.Job{Key: "w/1/x", ID: "1", Box: "w"}
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	for _, d := range []time.Duration{-110 * time.Minute, -100 * time.Minute, -55 * time.Minute} {
		h.Record(record{Type: recQueueAdd, At: at(d), Job: j})
	}
	h.Record(record{Type: recQueueCancel, At: at(-50 * time.Minute), Job: j})
	h.Record(record{Type: recQueueStart, At: at(-80 * time.Minute), Job: j, Seconds: 1800})
	for _, s := range []float64{600, 3000, 1200} {
		h.Record(record{Type: recQueueStart, At: at(-45 * time.Minute), Job: j, Seconds: s})
	}
	check := func(h *History) {
		t.Helper()
		r := h.Range(dash.RangeDay, Live{})
		want := map[int]dash.QueueCounts{21: {Jobs: 2, Started: 1, MedianWaitSeconds: 1800}, 22: {Jobs: 1, Started: 3, MedianWaitSeconds: 1200}}
		for i, b := range r.Buckets {
			if b.Queued != want[i] {
				t.Fatalf("bucket %d (%s): %+v, want %+v", i, b.Start.Format("15:04"), b.Queued, want[i])
			}
		}
		if w := h.Range(dash.RangeWeek, Live{}).Buckets[6].Queued; w.Jobs != 3 || w.Started != 4 || w.MedianWaitSeconds != 1500 {
			t.Fatalf("week: %+v", w)
		}
	}
	check(h)
	h2, err := OpenHistory(dir, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	check(h2)
}

// History written before game detection was removed (2026-11-25) holds mode
// records with a reason. They still read, and a yield joins the My PC
// stretch next to it.
func TestHistoryReadsOldModeRecordsWithAReason(t *testing.T) {
	dir := t.TempDir()
	lines := `{"v":1,"at":"2026-11-24T15:00:00Z","type":"mode","mode":{"mode":"shared","reason":""}}
{"v":1,"at":"2026-11-24T16:00:00Z","type":"mode","mode":{"mode":"mypc","reason":"gaming: steam:570"}}
{"v":1,"at":"2026-11-24T17:00:00Z","type":"mode","mode":{"mode":"mypc","reason":""}}
`
	if err := os.WriteFile(filepath.Join(dir, "events-2026-11-24.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := OpenHistory(dir, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	if m := h.LastMode(); m == nil || m.Mode != "mypc" {
		t.Fatalf("last mode %+v", m)
	}
	var segs []string
	for _, s := range h.Range(dash.RangeDay, Live{Mode: modeRec{Mode: "mypc"}}).Segments {
		segs = append(segs, s.Mode+"@"+s.Start.Format("15:04"))
	}
	if got := strings.Join(segs, ","); !strings.HasSuffix(got, "shared@15:00,mypc@16:00") {
		t.Fatalf("segments %s", got)
	}
}

// Existing history files may hold box-create records whose CreatedAt was zero
// or Unix epoch 0 while Incus was still creating the instance. Their span
// starts at the record's At time so they don't add phantom box-hours to every
// earlier hour and day.
func TestHistoryUnsetBoxCreatedAtAndBoxRepo(t *testing.T) {
	dir := t.TempDir()
	lines := `{"v":1,"at":"2026-11-24T16:10:00Z","type":"box-create","box":{"name":"zero","size":"medium","gpu":false,"createdAt":"0001-01-01T00:00:00Z"}}
{"v":1,"at":"2026-11-24T16:40:00Z","type":"box-delete","box":{"name":"zero","size":"medium","gpu":false,"createdAt":"0001-01-01T00:00:00Z"}}
{"v":1,"at":"2026-11-24T16:00:00Z","type":"box-create","box":{"name":"epoch-at","size":"medium","gpu":false,"createdAt":"1970-01-01T00:00:00Z"}}
{"v":1,"at":"2026-11-24T16:30:00Z","type":"box-delete","box":{"name":"epoch-at","size":"medium","gpu":false,"createdAt":"1970-01-01T00:00:00Z"}}
{"v":1,"at":"1970-01-01T00:00:00Z","type":"box-create","box":{"name":"epoch-raw","size":"medium","gpu":false,"createdAt":"1970-01-01T00:00:00Z"}}
{"v":1,"at":"2026-11-24T16:30:00Z","type":"box-delete","box":{"name":"epoch-raw","size":"medium","gpu":false,"createdAt":"1970-01-01T00:00:00Z"}}
`
	if err := os.WriteFile(filepath.Join(dir, "events-2026-11-24.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := OpenHistory(dir, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	r := h.Range(dash.RangeDay, Live{})
	if got := r.Buckets[0].BoxHours.BySize["medium"]; got != 0 {
		t.Fatalf("earliest bucket got phantom box-hours: %v", got)
	}
	if got := r.Buckets[21].BoxHours.BySize["medium"]; got != 1 {
		t.Fatalf("16:00 bucket box-hours = %v, want 1 (two 30m spans)", got)
	}
	w := h.Range(dash.RangeWeek, Live{})
	if got := w.Buckets[0].BoxHours.BySize["medium"]; got != 0 {
		t.Fatalf("week's first day got phantom box-hours: %v", got)
	}

	// BoxRepo recovers the newest job's repo for a box generation and ignores
	// jobs from an older generation of the same box name.
	old := dash.Job{Key: "b/1/x", ID: "1", Box: "b", State: dash.JobDone, StartedAt: t0.Add(-2 * time.Hour), Repo: "https://x/old/repo.git"}
	cur := dash.Job{Key: "b/2/x", ID: "2", Box: "b", State: dash.JobDone, StartedAt: t0.Add(-10 * time.Minute), Repo: "https://x/new/repo.git"}
	h.Record(record{Type: recJobEnd, At: old.StartedAt, Job: &old})
	h.Record(record{Type: recJobEnd, At: cur.StartedAt, Job: &cur})
	if got := h.BoxRepo("b", t0.Add(-15*time.Minute)); got != cur.Repo {
		t.Fatalf("BoxRepo current gen = %q, want %q", got, cur.Repo)
	}
	if got := h.BoxRepo("b", t0.Add(30*time.Minute)); got != "" {
		t.Fatalf("BoxRepo newer gen = %q, want empty", got)
	}
}

// The queue's eviction of a kept throwaway is in its jobs' lookup, also after
// boxd reopens the history; a job started later on a reused name isn't (#143).
func TestHistoryEvictedJob(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHistory(dir, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	ended := dash.Job{Key: "b/1/x", ID: "1", Box: "b", State: dash.JobDone, StartedAt: t0.Add(-10 * time.Minute)}
	h.Record(record{Type: recJobEnd, At: t0.Add(-5 * time.Minute), Job: &ended})
	h.Record(record{Type: recControl, At: t0, Control: &controlRec{Action: actionEvict, Target: "b", By: "q.20261124-160000-ab12"}})
	later := dash.Job{Key: "b/2/y", ID: "2", Box: "b", State: dash.JobDone, StartedAt: t0.Add(time.Minute)}
	h.Record(record{Type: recJobEnd, At: t0.Add(2 * time.Minute), Job: &later})
	if h, err = OpenHistory(dir, func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}
	if j, ok := h.JobByID("b", "1"); !ok || j.Evicted != "q.20261124-160000-ab12" {
		t.Errorf("evicted job = %+v, %v", j, ok)
	}
	if j, _ := h.JobByID("b", "2"); j.Evicted != "" {
		t.Errorf("a later job on the name was marked evicted: %q", j.Evicted)
	}
}
