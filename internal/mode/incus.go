package mode

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Instance is one Incus instance in the agents project.
type Instance struct {
	Name   string `json:"name"`
	Status string `json:"status"` // Running, Frozen, Stopped, ...
	// Config is the instance's own config (not the profile's), as
	// `incus list --format json` reports it. The Manager compares it with
	// what the mode wants, so a key an agent changed is put back.
	Config map[string]string `json:"config"`
	// ExpandedConfig adds what profiles give it; Incus counts limits.memory
	// from here when it checks the project cap.
	ExpandedConfig map[string]string `json:"expanded_config"`
}

// Incus is everything the Manager needs from Incus.
type Incus interface {
	List(ctx context.Context) ([]Instance, error)
	Pause(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	// Delete removes the instance and its disk (forced, so a frozen or running one goes too).
	Delete(ctx context.Context, name string) error
	SetInstanceConfig(ctx context.Context, name, key, value string) error
	UnsetInstanceConfig(ctx context.Context, name, key string) error
	// ProjectConfig reads one key of the project's config ("" when unset).
	ProjectConfig(ctx context.Context, key string) (string, error)
	// SetProjectConfig sets the mode label, the yield reason and the memory
	// cap (MemoryCapKey). Incus refuses to lower a cap below what existing
	// instances already hold, so the Manager never asks it to.
	SetProjectConfig(ctx context.Context, key, value string) error
}

// Project is the Incus project agents live in.
const Project = "agents"

// RunFunc runs `incus <args...>` and returns its stdout. On Windows it goes
// through `wsl.exe -d agents -- incus ...`; tests pass a fake.
type RunFunc func(ctx context.Context, args ...string) ([]byte, error)

// CLI implements Incus by shelling out to the incus command.
type CLI struct{ Run RunFunc }

func (c CLI) do(ctx context.Context, args ...string) error {
	_, err := c.Run(ctx, args...)
	return err
}

func (c CLI) List(ctx context.Context) ([]Instance, error) {
	out, err := c.Run(ctx, "list", "--project", Project, "--format", "json")
	if err != nil {
		return nil, err
	}
	var ins []Instance
	if err := json.Unmarshal(out, &ins); err != nil {
		return nil, fmt.Errorf("parse incus list: %w", err)
	}
	return ins, nil
}

func (c CLI) Pause(ctx context.Context, name string) error {
	return c.do(ctx, "pause", name, "--project", Project)
}

// Resume thaws a frozen instance; `incus start` does that.
func (c CLI) Resume(ctx context.Context, name string) error {
	return c.do(ctx, "start", name, "--project", Project)
}

// Stop is forced because a frozen instance won't stop gracefully.
func (c CLI) Stop(ctx context.Context, name string) error {
	return c.do(ctx, "stop", name, "--force", "--project", Project)
}

// Delete is forced because the reaper deletes running and frozen instances.
func (c CLI) Delete(ctx context.Context, name string) error {
	return c.do(ctx, "delete", name, "--force", "--project", Project)
}

func (c CLI) SetInstanceConfig(ctx context.Context, name, key, value string) error {
	return c.do(ctx, "config", "set", name, key+"="+value, "--project", Project)
}

func (c CLI) UnsetInstanceConfig(ctx context.Context, name, key string) error {
	return c.do(ctx, "config", "unset", name, key, "--project", Project)
}

func (c CLI) ProjectConfig(ctx context.Context, key string) (string, error) {
	out, err := c.Run(ctx, "project", "get", Project, key)
	return strings.TrimSpace(string(out)), err
}

func (c CLI) SetProjectConfig(ctx context.Context, key, value string) error {
	return c.do(ctx, "project", "set", Project, key+"="+value)
}

// Reclaimer pushes a frozen instance's memory out to swap, so the WSL VM can
// give the RAM back to Windows while a game runs.
type Reclaimer interface {
	Reclaim(ctx context.Context, name string) error
}

// CgroupReclaim implements Reclaimer with cgroup v2's memory.reclaim on the
// container's cgroup. Run runs a command as root inside the distro.
//
// Incus gives every container memory.swap.max=0 and a restricted project
// forbids limits.memory.swap, so the script opens swap only for the reclaim
// and then puts the old value back: pages already out stay out, and a
// running box never swaps. memory.reclaim works on a frozen cgroup, since
// the writer does the reclaim; it ends with EAGAIN when it can't take all it
// was asked for (a few MB are always left), which is fine. Measured on the PC
// on 2026-11-24: 3.2GB went out in 3.2s and Windows had it back within 30s.
type CgroupReclaim struct{ Run RunFunc }

// reclaimScript gets the instance name as $1. It has no double quotes or
// backslashes, so it passes through wsl.exe's command line intact.
const reclaimScript = `c=/sys/fs/cgroup/lxc.payload.` + Project + `_$1; ` +
	`s=$(cat $c/memory.swap.max) || exit 1; echo max > $c/memory.swap.max; ` +
	`cat $c/memory.current > $c/memory.reclaim 2>/dev/null; echo $s > $c/memory.swap.max`

var instanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

func (c CgroupReclaim) Reclaim(ctx context.Context, name string) error {
	if !instanceName.MatchString(name) {
		return fmt.Errorf("reclaim: bad instance name %q", name)
	}
	if _, err := c.Run(ctx, "sh", "-c", reclaimScript, "sh", name); err != nil {
		return fmt.Errorf("reclaim %s: %w", name, err)
	}
	return nil
}
