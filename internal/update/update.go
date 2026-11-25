// Package update finds agent-box's GitHub releases, downloads a release's asset checked against
// its SHA256SUMS, and orders release tags (docs/releasing.md). It knows nothing about which part
// it updates: box's self-update is in cmd/box/update.go.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Repo is where releases are published.
const Repo = "a-Fig/agent-box"

// Release is a published GitHub release: its tag, and its assets' download URLs by name.
type Release struct {
	Tag    string
	Assets map[string]string
}

// Client talks to GitHub's API. The zero value uses api.github.com and http.DefaultClient.
type Client struct {
	API  string // base URL, for tests
	HTTP *http.Client
}

// Latest is GitHub's latest release, which is never a prerelease: the stable channel.
func (c Client) Latest(ctx context.Context) (Release, error) {
	return c.release(ctx, "/repos/"+Repo+"/releases/latest")
}

// ByTag is the release with that tag, a nightly included.
func (c Client) ByTag(ctx context.Context, tag string) (Release, error) {
	if !ValidTag(tag) {
		return Release{}, fmt.Errorf("%q isn't a release tag like v0.3.0 or v0.3.1-nightly.20261125.1", tag)
	}
	return c.release(ctx, "/repos/"+Repo+"/releases/tags/"+tag)
}

func (c Client) release(ctx context.Context, path string) (Release, error) {
	api := c.API
	if api == "" {
		api = "https://api.github.com"
	}
	b, err := c.get(ctx, api+path, 1<<20)
	if err != nil {
		return Release{}, err
	}
	var r struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Release{}, fmt.Errorf("release: %w", err)
	}
	rel := Release{Tag: r.TagName, Assets: map[string]string{}}
	for _, a := range r.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel, nil
}

// Asset downloads a release's asset and checks it against the release's SHA256SUMS. The sum
// catches a damaged download; it doesn't prove who built the file.
func (c Client) Asset(ctx context.Context, rel Release, name string) ([]byte, error) {
	url, ok := rel.Assets[name]
	sumsURL, sok := rel.Assets["SHA256SUMS"]
	if !ok || !sok {
		return nil, fmt.Errorf("release %s has no %s or no SHA256SUMS", rel.Tag, name)
	}
	sums, err := c.get(ctx, sumsURL, 1<<20)
	if err != nil {
		return nil, err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() { // sha256sum's format: the hash, two spaces (or " *"), the name
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("SHA256SUMS of %s doesn't list %s", rel.Tag, name)
	}
	data, err := c.get(ctx, url, 256<<20)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(data); !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return nil, fmt.Errorf("%s of %s doesn't match SHA256SUMS", name, rel.Tag)
	}
	return data, nil
}

func (c Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "agent-box-updater")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("GET %s: over %d bytes", url, limit)
	}
	return b, err
}

var tagRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-nightly\.(\d{8})\.(\d+))?$`)

// ValidTag says tag is a release tag: vX.Y.Z, or vX.Y.Z-nightly.YYYYMMDD.N.
func ValidTag(tag string) bool { return tagRE.MatchString(tag) }

// After says release a comes after b. A tag that isn't a release's (a dev build's "") never
// comes after anything, and anything valid comes after it. A nightly of X.Y.Z comes before
// X.Y.Z itself.
func After(a, b string) bool {
	ka, oka := key(a)
	kb, okb := key(b)
	if !oka {
		return false
	}
	if !okb {
		return true
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return ka[i] > kb[i]
		}
	}
	return false
}

// key orders a tag: X, Y, Z, then a stable release above all of its nightlies, then the
// nightly's date and number.
func key(tag string) ([6]int, bool) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return [6]int{}, false
	}
	var k [6]int
	for i, s := range []string{m[1], m[2], m[3]} {
		k[i], _ = strconv.Atoi(s)
	}
	if m[4] == "" {
		k[3] = 1
	} else {
		k[4], _ = strconv.Atoi(m[4])
		k[5], _ = strconv.Atoi(m[5])
	}
	return k, true
}
