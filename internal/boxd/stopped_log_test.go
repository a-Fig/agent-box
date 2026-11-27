package boxd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

type fileRangeTransport func(*http.Request) (*http.Response, error)

func (f fileRangeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIncusFileRangeUsesServeContent(t *testing.T) {
	log := []byte("first\x00bad\xffbytes")
	socket := &Socket{Proj: "agents team", client: &http.Client{Transport: fileRangeTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/1.0/instances/repo-123/files" ||
			req.URL.Query().Get("project") != "agents team" || req.URL.Query().Get("path") != "/work/.box/job.log" {
			t.Fatalf("file request = %s %s", req.Method, req.URL)
		}
		rec := httptest.NewRecorder()
		rec.Header().Set("X-Incus-type", "file")
		http.ServeContent(rec, req, "job.log", time.Time{}, bytes.NewReader(log))
		return rec.Result(), nil
	})}}
	for _, tc := range []struct {
		offset, limit int64
		want          string
	}{
		{0, 5, "first"},
		{5, 5, "\x00bad\xff"},
		{10, 64, "bytes"},
		{15, 6, ""},
		{25, 6, ""},
	} {
		got, size, err := socket.FileRange(context.Background(), "repo-123", "/work/.box/job.log", tc.offset, tc.limit)
		if err != nil || string(got) != tc.want || size != int64(len(log)) {
			t.Fatalf("range %d+%d = %q of %d, %v", tc.offset, tc.limit, got, size, err)
		}
	}
	log = nil
	if got, size, err := socket.FileRange(context.Background(), "repo-123", "/work/.box/job.log", 0, 2); err != nil || len(got) != 0 || size != 0 {
		t.Fatalf("empty file = %q of %d, %v", got, size, err)
	}
}

func TestIncusFileRangeRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, header, body, fileType string
		status                       int
	}{
		{"ignored range", "", "the whole file", "file", 200},
		{"wrong start", "bytes 0-1/8", "ab", "file", 206},
		{"short range", "bytes 2-2/8", "c", "file", 206},
		{"short body", "bytes 2-3/8", "c", "file", 206},
		{"overlong body", "bytes 2-3/8", "cde", "file", 206},
		{"offset is in file", "bytes */8", "", "file", 416},
		{"symlink", "bytes 2-3/8", "cd", "symlink", 206},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := &Socket{client: &http.Client{Transport: fileRangeTransport(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Range") != "bytes=2-3" {
					t.Fatalf("range request = %q", req.Header.Get("Range"))
				}
				rec := httptest.NewRecorder()
				rec.Header().Set("X-Incus-type", tc.fileType)
				rec.Header().Set("Content-Range", tc.header)
				rec.WriteHeader(tc.status)
				fmt.Fprint(rec, tc.body)
				return rec.Result(), nil
			})}}
			if got, _, err := socket.FileRange(context.Background(), "repo-123", "/work/.box/job.log", 2, 2); err == nil || len(got) != 0 {
				t.Fatalf("invalid response yielded %q, %v", got, err)
			}
		})
	}
}

type interceptFileRange struct {
	Incus
	afterRead func()
	err       error
}

func (i interceptFileRange) FileRange(ctx context.Context, box, path string, offset, limit int64) ([]byte, int64, error) {
	if i.err != nil {
		return nil, 0, i.err
	}
	data, size, err := i.Incus.FileRange(ctx, box, path, offset, limit)
	i.afterRead()
	return data, size, err
}

func TestStoppedJobLogPagesMatchingInstance(t *testing.T) {
	const job = "w.20261124-180100-ab12"
	c := &clock{t: t0.Add(time.Minute)}
	log := "aé\xff" + strings.Repeat("long test output\n", 6000)
	in := inst("w", "Stopped", "small", t0)
	in.Config["volatile.uuid"] = "original"
	f := &fakeIncus{
		ins:  []Instance{in},
		cfg:  map[string]string{"user.agent-box.mode": "full"},
		jobs: map[string][]fakeJob{"w": {{name: job, state: "done", exit: "1", log: log, meta: metaJSON}}},
	}
	w := newWatcher(t, t.TempDir(), f, c)
	started, ended, exit := c.now(), c.now().Add(time.Second), 1
	j := dash.Job{Box: "w", ID: "20261124-180100-ab12", Machine: "pc", BoxUID: "original", State: dash.JobDone,
		StartedAt: started, EndedAt: &ended, Exit: &exit}
	j.Key = dash.JobKey(j.Box, j.ID, j.StartedAt)
	if err := w.Hist.Record(record{Type: recJobEnd, At: ended, Job: &j}); err != nil {
		t.Fatal(err)
	}
	w.Poll(context.Background())
	h := (&Server{W: w}).Handler()
	path := "/api/jobs/" + url.PathEscape(j.Key) + "/log"
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+query, nil))
		return rec
	}
	decode := func(result *httptest.ResponseRecorder) dash.LogChunk {
		t.Helper()
		var chunk dash.LogChunk
		if err := json.Unmarshal(result.Body.Bytes(), &chunk); result.Code != http.StatusOK || err != nil {
			t.Fatalf("response %d %s, %v", result.Code, result.Body, err)
		}
		return chunk
	}

	var rebuilt strings.Builder
	offset := int64(0)
	for _, limit := range []int{2, dash.MaxLogChunk, dash.MaxLogChunk} {
		chunk := decode(get(fmt.Sprintf("?stopped=1&offset=%d&limit=%d", offset, limit)))
		payload := chunk.Data
		if chunk.DataBase64 != "" {
			raw, err := base64.StdEncoding.DecodeString(chunk.DataBase64)
			if err != nil {
				t.Fatal(err)
			}
			payload = string(raw)
		}
		if !chunk.Stopped || chunk.LogBytes != int64(len(log)) || offset+int64(len(payload)) != chunk.Offset ||
			chunk.EOF != (chunk.Offset == int64(len(log))) {
			t.Fatalf("page after %d = %+v", offset, chunk)
		}
		rebuilt.WriteString(payload)
		offset = chunk.Offset
	}
	if rebuilt.String() != log {
		t.Fatal("stopped pages changed the log bytes")
	}
	if dashChunk := decode(get("")); !dashChunk.Stopped || dashChunk.Offset == 0 {
		t.Fatalf("dashboard page = %+v", dashChunk)
	}
	if r := get("?stopped=1&limit=65537"); r.Code != http.StatusBadRequest {
		t.Fatalf("overlong page = %d %s", r.Code, r.Body)
	}

	w.Incus = interceptFileRange{Incus: f, err: errors.New("files subsystem offline")}
	if r := get("?stopped=1"); r.Code != http.StatusBadGateway || strings.Contains(r.Body.String(), "long test output") {
		t.Fatalf("file read failure = %d %s", r.Code, r.Body)
	}
	w.Incus = interceptFileRange{Incus: f, afterRead: func() {
		f.set(func() { f.ins[0].Config["volatile.uuid"] = "replacement" })
	}}
	if r := get("?stopped=1"); r.Code != http.StatusNotFound || strings.Contains(r.Body.String(), "long test output") {
		t.Fatalf("replaced during read = %d %s", r.Code, r.Body)
	}
	reads := f.fileReads
	if r := get("?stopped=1"); r.Code != http.StatusNotFound || f.fileReads != reads {
		t.Fatalf("replaced before read = %d %s, file reads %d to %d", r.Code, r.Body, reads, f.fileReads)
	}
}
