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
	// CLI runs `incus <args...>` in the project, with stdin (often ""), for
	// what starting a queued job takes: launch, file push, config set, exec.
	CLI(ctx context.Context, stdin string, args ...string) ([]byte, error)
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
