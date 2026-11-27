package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
)

// Incus is what boxd needs from Incus, all of it in project agents. The real
// one is Socket; tests use a fake.
type Incus interface {
	// Instances lists the project's instances with their state.
	Instances(ctx context.Context) ([]Instance, error)
	// Project reads the project's config and its resource counters.
	Project(ctx context.Context) (Project, error)
	// Exec runs a bash script in a running instance and returns its stdout.
	// A non-zero exit is an *ExitError.
	Exec(ctx context.Context, name, script string, args ...string) ([]byte, error)
	// FileRange reads a bounded byte range without starting the instance.
	// It returns the full file size, including when offset is past EOF.
	FileRange(ctx context.Context, name, path string, offset, limit int64) ([]byte, int64, error)
	// InstanceBusy reports whether Incus has a pending operation for the instance.
	InstanceBusy(ctx context.Context, name string) (bool, error)
	// CLI runs `incus <args...>` in the project, with stdin (often ""), for
	// what starting a queued job takes: launch, file push, config set, exec.
	CLI(ctx context.Context, stdin string, args ...string) ([]byte, error)
	// SetConfigIf merges the chosen config keys while the instance's ETag
	// matches the read. It reads and decides again on a conflict. A nil map
	// or a missing instance skips the write.
	SetConfigIf(ctx context.Context, name string, decide func(Instance) map[string]string) (bool, error)
}

// Instance is the part of GET /1.0/instances?recursion=2 boxd reads.
type Instance struct {
	Name           string            `json:"name"`
	Status         string            `json:"status"` // Running, Frozen, Stopped, ...
	Config         map[string]string `json:"config"`
	ExpandedConfig map[string]string `json:"expanded_config"`
	CreatedAt      time.Time         `json:"created_at"`
	State          *struct {
		Pid    int64 `json:"pid"`
		Memory struct {
			Usage int64 `json:"usage"`
		} `json:"memory"`
		CPU struct {
			Usage int64 `json:"usage"` // nanoseconds of CPU time
		} `json:"cpu"`
	} `json:"state"`
}

// Project is the project's config and GET /1.0/projects/agents/state.
type Project struct {
	Config    map[string]string
	Resources map[string]struct {
		Limit int64 `json:"limit"`
		Usage int64 `json:"usage"`
	}
}

// ExitError is a script that exited non-zero.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit %d: %s", e.Code, strings.TrimSpace(e.Stderr))
}

// Socket talks to Incus's REST API on its unix socket, and runs exec through
// the incus command: exec over REST needs a WebSocket client, which the
// standard library doesn't have (docs/ui.md, "Watching Incus").
type Socket struct {
	Path   string // /var/lib/incus/unix.socket
	Proj   string // the project, agents
	client *http.Client
}

// NewSocket makes a client for the socket at path.
func NewSocket(path, project string) *Socket {
	return &Socket{Path: path, Proj: project, client: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}}
}

func (s *Socket) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://incus"+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Error    string          `json:"error"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("incus %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("incus %s: %s: %s", path, resp.Status, env.Error)
	}
	return json.Unmarshal(env.Metadata, out)
}

func (s *Socket) Instances(ctx context.Context) ([]Instance, error) {
	var ins []Instance
	err := s.get(ctx, "/1.0/instances?recursion=2&project="+url.QueryEscape(s.Proj), &ins)
	return ins, err
}

func (s *Socket) InstanceBusy(ctx context.Context, name string) (bool, error) {
	var ops map[string][]struct {
		Resources map[string][]string `json:"resources"`
	}
	if err := s.get(ctx, "/1.0/operations?recursion=1&project="+url.QueryEscape(s.Proj), &ops); err != nil {
		return false, err
	}
	if ops == nil {
		return false, errors.New("Incus did not return an operation list")
	}
	for status, group := range ops {
		if status == "success" || status == "failure" || status == "cancelled" {
			continue
		}
		for _, op := range group {
			for _, resource := range op.Resources["instances"] {
				u, err := url.Parse(resource)
				if err != nil {
					return false, err
				}
				if u.Path == "/1.0/instances/"+name {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (s *Socket) Project(ctx context.Context) (Project, error) {
	var p Project
	var cfg struct {
		Config map[string]string `json:"config"`
	}
	if err := s.get(ctx, "/1.0/projects/"+url.PathEscape(s.Proj), &cfg); err != nil {
		return p, err
	}
	var st struct {
		Resources map[string]struct {
			Limit int64 `json:"limit"`
			Usage int64 `json:"usage"`
		} `json:"resources"`
	}
	if err := s.get(ctx, "/1.0/projects/"+url.PathEscape(s.Proj)+"/state", &st); err != nil {
		return p, err
	}
	p.Config, p.Resources = cfg.Config, st.Resources
	return p, nil
}

func (s *Socket) Exec(ctx context.Context, name, script string, args ...string) ([]byte, error) {
	argv := append([]string{"exec", "--project", s.Proj, name, "--", "bash", "-c", script, "boxd"}, args...)
	cmd := exec.CommandContext(ctx, "incus", argv...)
	stdout, stderr := &capped{max: maxExecOut}, &capped{max: 64 << 10}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stdout, stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return stdout.Bytes(), &ExitError{Code: ee.ExitCode(), Stderr: string(stderr.Bytes())}
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("incus exec %s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

// errStale is a write Incus refused with 412: the instance changed since it
// was read.
var errStale = errors.New("ETag doesn't match")

func (s *Socket) SetConfigIf(ctx context.Context, name string, decide func(Instance) map[string]string) (bool, error) {
	path := "/1.0/instances/" + url.PathEscape(name) + "?project=" + url.QueryEscape(s.Proj)
	var wrote bool
	err := jobrun.RetryETag(ctx, func() error {
		wrote = false
		req, err := http.NewRequestWithContext(ctx, "GET", "http://incus"+path, nil)
		if err != nil {
			return err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return err
		}
		var env struct {
			Error    string   `json:"error"`
			Metadata Instance `json:"metadata"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&env)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("incus GET %s: %s: %s", name, resp.Status, env.Error)
		case derr != nil:
			return fmt.Errorf("incus GET %s: %w", name, derr)
		}
		config := decide(env.Metadata)
		if len(config) == 0 {
			return nil
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return fmt.Errorf("incus GET %s: missing ETag", name)
		}
		body, _ := json.Marshal(map[string]any{"config": config})
		patch, err := http.NewRequestWithContext(ctx, "PATCH", "http://incus"+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		patch.Header.Set("Content-Type", "application/json")
		patch.Header.Set("If-Match", etag)
		presp, err := s.client.Do(patch)
		if err != nil {
			return err
		}
		var penv struct {
			Error string `json:"error"`
		}
		json.NewDecoder(presp.Body).Decode(&penv)
		presp.Body.Close()
		switch presp.StatusCode {
		case http.StatusOK:
			wrote = true
			return nil
		case http.StatusPreconditionFailed:
			return fmt.Errorf("incus PATCH %s: %w", name, errStale)
		case http.StatusNotFound:
			return nil
		}
		return fmt.Errorf("incus PATCH %s: %s: %s", name, presp.Status, penv.Error)
	})
	return wrote, err
}

// maxExecOut bounds what boxd reads from a box: a scan of maxScanJobs jobs
// with their meta and tails, or a log chunk, is well under it.
const maxExecOut = 8 << 20

// capped keeps the first max bytes written and drops the rest, so a box
// can't make boxd hold more.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (s *Socket) CLI(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("no incus command")
	}
	argv := append([]string{args[0], "--project", s.Proj}, args[1:]...)
	cmd := exec.CommandContext(ctx, "incus", argv...)
	stdout, stderr := &capped{max: maxExecOut}, &capped{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin) // else /dev/null, so incus never waits on YAML
	}
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("incus %s: %v: %s", args[0], err, strings.TrimSpace(string(stderr.Bytes())))
	}
	return stdout.Bytes(), nil
}
