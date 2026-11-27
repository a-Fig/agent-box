package boxd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-Fig/agent-box/internal/dash"
)

func (s *Socket) FileRange(ctx context.Context, name, path string, offset, limit int64) ([]byte, int64, error) {
	if offset < 0 || limit < 1 || limit > dash.MaxLogChunk || offset > math.MaxInt64-limit {
		return nil, 0, errors.New("invalid file range")
	}
	query := url.Values{"project": {s.Proj}, "path": {path}}
	address := "http://incus/1.0/instances/" + url.PathEscape(name) + "/files?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+limit-1))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, 0, fs.ErrNotExist
	}
	if resp.Header.Get("X-Incus-type") != "file" {
		return nil, 0, errors.New("Incus did not return a file")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, 0, errors.New("Incus encoded the file response")
	}
	header := resp.Header.Get("Content-Range")
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		total, ok := strings.CutPrefix(header, "bytes */")
		size, err := strconv.ParseInt(total, 10, 64)
		if ok && err == nil && size >= 0 && offset >= size {
			return nil, size, nil
		}
		return nil, 0, errors.New("Incus returned an invalid unsatisfied range")
	}
	// ServeContent may ignore Range on an empty file.
	if resp.StatusCode == http.StatusOK && resp.ContentLength == 0 {
		return nil, 0, nil
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, 0, fmt.Errorf("Incus did not honor the file range (%s)", resp.Status)
	}
	span, total, ok := strings.Cut(header, "/")
	size, err := strconv.ParseInt(total, 10, 64)
	if !ok || err != nil || size <= offset {
		return nil, 0, errors.New("Incus returned an invalid file size")
	}
	want := min(limit, size-offset)
	if span != fmt.Sprintf("bytes %d-%d", offset, offset+want-1) {
		return nil, 0, errors.New("Incus returned an unexpected file range")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) != want {
		return nil, 0, errors.New("Incus returned the wrong number of file bytes")
	}
	return data, size, nil
}
