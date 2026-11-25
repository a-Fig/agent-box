package boxd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

// The queue (docs/queue.md): a run that can't start now waits in boxd, on a
// snapshot of the worktree as it was when it was queued, and boxd starts it
// the way box run does once it can. Each job is <job>.json plus its
// <job>.bundle in Dir, so a restart reloads it.

// queued is one waiting job, as its JSON file holds it.
type queued struct {
	Job      string            `json:"job"` // <box>.<id>
	Box      string            `json:"box"`
	ID       string            `json:"id"`
	New      bool              `json:"new"`
	Size     string            `json:"size"`
	GPU      bool              `json:"gpu"`
	Checkout string            `json:"checkout"`
	Ref      string            `json:"ref"`
	Command  []string          `json:"command"`
	Env      map[string]string `json:"env"`
	Timeout  int               `json:"timeout"`
	Meta     json.RawMessage   `json:"meta"`
	QueuedAt time.Time         `json:"queuedAt"`
	Order    int64             `json:"order"` // lower starts first; front moves a job below the rest
	Bytes    int64             `json:"bytes"` // the bundle's size
	// Starting is when a start began, written before it launches a box or
	// runs anything, and kept until the job leaves the queue. A boxd that
	// restarts finds it and looks in the box before starting the job again,
	// so a start cut short never runs the job twice.
	Starting *time.Time `json:"starting,omitempty"`

	view     dash.Job // the job as the page sees it, from Meta
	starting bool
	evict    []string // kept throwaways to delete for its room (spareThrowaways)
}

// Queue is the store. Its methods are safe to call at once.
type Queue struct {
	dir string
	max int64 // dash.QueueBytes; tests set less

	mu    sync.Mutex
	items map[string]*queued
}

// OpenQueue loads the queue in dir, dropping a job whose bundle is missing
// and a bundle whose job is.
func OpenQueue(dir string) (*Queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	q := &Queue{dir: dir, max: dash.QueueBytes, items: map[string]*queued{}}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		var it queued
		if err != nil || json.Unmarshal(b, &it) != nil || !validJob.MatchString(it.Job) {
			os.Remove(f)
			continue
		}
		if _, err := os.Stat(q.bundle(it.Job)); err != nil {
			os.Remove(f)
			continue
		}
		it.view = viewOf(&it)
		q.items[it.Job] = &it
	}
	bundles, _ := filepath.Glob(filepath.Join(dir, "*.bundle"))
	for _, b := range bundles {
		if _, ok := q.items[strings.TrimSuffix(filepath.Base(b), ".bundle")]; !ok {
			os.Remove(b)
		}
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	for _, t := range tmps {
		os.Remove(t)
	}
	return q, nil
}

func (q *Queue) bundle(job string) string { return filepath.Join(q.dir, job+".bundle") }

// BundlePath is where a waiting job's snapshot is.
func (q *Queue) BundlePath(job string) string { return q.bundle(job) }

var (
	validBoxName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	validCheckout = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)
	validRef      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	validEnvName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretEnvName = regexp.MustCompile(`TOKEN|SECRET|KEY|PASSWORD|PASSWD|CREDENTIAL`) // box's own rule
)

// errQueueFull is a snapshot that would take the queue past its bytes.
var errQueueFull = errors.New("the queue's snapshots would pass 2GiB")

// check validates a request: what reaches a shell in the box is names and
// quoted values only.
func check(r *dash.QueueRequest) error {
	if r.Size != "" {
		sz, ok := mode.LookupSize(r.Size)
		if !ok {
			return fmt.Errorf("unknown size %q", r.Size)
		}
		r.GPU = r.GPU || sz.GPU
	}
	if r.GPU {
		r.Size, r.New = mode.GPUSize, true // a GPU run always gets a throwaway, as box run does
	}
	if r.New && len(r.Box) > 50 { // room for the random suffix
		r.Box = strings.TrimRight(r.Box[:50], "-")
	}
	if m := strings.TrimSpace(string(r.Meta)); m == "" || m == "null" {
		r.Meta = json.RawMessage("{}")
	}
	var obj map[string]any
	switch {
	case !validBoxName.MatchString(r.Box):
		return fmt.Errorf("bad box name %q", r.Box)
	case !validCheckout.MatchString(r.Checkout):
		return fmt.Errorf("bad checkout %q", r.Checkout)
	case !validRef.MatchString(r.Ref):
		return fmt.Errorf("bad ref %q", r.Ref)
	case len(r.Command) == 0 || r.Command[0] == "":
		return errors.New("no command")
	case r.Timeout < 0 || r.Timeout > 7*24*3600:
		return errors.New("timeout is 0 to 7 days")
	case len(r.Meta) > 15<<10: // boxd adds to it, and the scan reads 16KiB
		return errors.New("meta is over 15KiB")
	case json.Unmarshal(r.Meta, &obj) != nil || obj == nil:
		return errors.New("meta must be a JSON object")
	}
	for k, v := range r.Env {
		if !validEnvName.MatchString(k) || secretEnvName.MatchString(strings.ToUpper(k)) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("env %s isn't allowed", k)
		}
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Add stores a checked request and its bundle, read from body.
func (q *Queue) Add(r dash.QueueRequest, body io.Reader, now time.Time) (*queued, error) {
	now = now.UTC()
	it := &queued{Box: r.Box, New: r.New, Size: r.Size, GPU: r.GPU, Checkout: r.Checkout, Ref: r.Ref,
		Command: r.Command, Env: r.Env, Timeout: r.Timeout, Meta: r.Meta, QueuedAt: now}
	if it.New {
		it.Box = r.Box + "-" + randHex(2)
	}
	it.ID = now.Format("20060102-150405") + "-" + randHex(2)
	it.Job = it.Box + "." + it.ID

	q.mu.Lock()
	var used int64
	for _, o := range q.items {
		used += o.Bytes
		it.Order = max(it.Order, o.Order+1)
	}
	q.mu.Unlock()
	tmp := q.bundle(it.Job) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, io.LimitReader(body, q.max-used+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && used+n > q.max {
		err = errQueueFull
	}
	if err == nil && n == 0 {
		err = errors.New("no bundle")
	}
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	it.Bytes = n
	if err := os.Rename(tmp, q.bundle(it.Job)); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	it.view = viewOf(it)

	q.mu.Lock()
	defer q.mu.Unlock()
	// Another Add may have filled the queue meanwhile.
	used = 0
	for _, o := range q.items {
		used += o.Bytes
	}
	if used+n > q.max {
		os.Remove(q.bundle(it.Job))
		return nil, errQueueFull
	}
	if err := q.save(it); err != nil {
		os.Remove(q.bundle(it.Job))
		return nil, err
	}
	q.items[it.Job] = it
	return it, nil
}

// save writes a job's JSON file, whole or not at all. Called with mu held.
func (q *Queue) save(it *queued) error {
	b, err := json.Marshal(it)
	if err != nil {
		return err
	}
	p := filepath.Join(q.dir, it.Job+".json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// Remove drops a job and its snapshot, and says whether it was there.
func (q *Queue) Remove(job string) (*queued, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	it, ok := q.items[job]
	if !ok {
		return nil, false
	}
	delete(q.items, job)
	os.Remove(filepath.Join(q.dir, job+".json"))
	os.Remove(q.bundle(job))
	return it, true
}

// Front moves a job ahead of the rest.
func (q *Queue) Front(job string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	it, ok := q.items[job]
	if !ok {
		return false
	}
	for _, o := range q.items {
		if o != it {
			it.Order = min(it.Order, o.Order-1)
		}
	}
	q.save(it)
	return true
}

// list is the waiting jobs in their order. The pointers are the store's;
// callers hold no lock, so they read only fields that don't change.
func (q *Queue) list() []*queued {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*queued, 0, len(q.items))
	for _, it := range q.items {
		out = append(out, it)
	}
	slices.SortFunc(out, func(a, b *queued) int {
		if a.Order != b.Order {
			return int(a.Order - b.Order)
		}
		return strings.Compare(a.Job, b.Job)
	})
	return out
}

func (q *Queue) get(job string) (*queued, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	it, ok := q.items[job]
	return it, ok
}

// markStarting records on disk that a start of it began at t.
func (q *Queue) markStarting(it *queued, t time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if it.Starting == nil {
		it.Starting = &t
	}
	return q.save(it)
}

func (q *Queue) setStarting(it *queued, v bool) {
	q.mu.Lock()
	it.starting = v
	q.mu.Unlock()
}

func (q *Queue) isStarting(it *queued) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return it.starting
}

// viewOf is the job as the page sees it while it waits.
func viewOf(it *queued) dash.Job {
	at := it.QueuedAt
	j := dash.Job{Key: dash.JobKey(it.Box, it.ID, at), ID: it.ID, Box: it.Box, State: dash.JobQueued,
		StartedAt: at, QueuedAt: &at, Size: it.Size, GPU: it.GPU, Command: strings.Join(it.Command, " ")}
	if j.Size == "" {
		j.Size = mode.DefaultSize
	}
	var m meta
	if json.Unmarshal(it.Meta, &m) == nil {
		fillMeta(&j, &m)
		j.Size, j.GPU = it.Size, it.GPU
		if j.Size == "" {
			j.Size = mode.DefaultSize
		}
	}
	j.Kind = dash.Classify(j.Command, j.GPU)
	return j
}
