package mode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Instance is one Incus instance in the agents project.
type Instance struct {
	Name   string `json:"name"`
	Status string `json:"status"` // Running, Frozen, Stopped, ...
	// LastUsedAt is when the instance was last started. Incus sets it to the
	// Unix epoch (1970-01-01T00:00:00Z) when CreateInternal inserts the row,
	// before the image is unpacked and before the first start.
	LastUsedAt time.Time `json:"last_used_at"`
	// Config is the instance's own config (not the profile's), as
	// `incus list --format json` reports it. The Manager compares it with
	// what the mode wants, so a key an agent changed is put back.
	Config map[string]string `json:"config"`
	// ExpandedConfig adds what profiles give it; Incus counts limits.memory
	// from here when it checks the project cap.
	ExpandedConfig map[string]string `json:"expanded_config"`
	// State is nil for a stopped instance. Pid is its init process on the host.
	State *InstanceState `json:"state"`
}

// InstanceState is the part of `incus list --format json`'s state used here.
type InstanceState struct {
	Pid int64 `json:"pid"`
}

// Incus is everything the Manager needs from Incus.
type Incus interface {
	List(ctx context.Context) ([]Instance, error)
	Pause(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	// Delete removes the instance and its disk (forced, so a frozen or running one goes too).

	SetInstanceConfig(ctx context.Context, name, key, value string) error
	// ProjectConfig reads one key of the project's config ("" when unset).
	ProjectConfig(ctx context.Context, key string) (string, error)
	// SetProjectConfig sets the mode label, the handshake keys and the memory
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

func (c CLI) SetInstanceConfig(ctx context.Context, name, key, value string) error {
	return c.do(ctx, "config", "set", name, key+"="+value, "--project", Project)
}

func (c CLI) ProjectConfig(ctx context.Context, key string) (string, error) {
	out, err := c.Run(ctx, "project", "get", Project, key)
	return strings.TrimSpace(string(out)), err
}

func (c CLI) SetProjectConfig(ctx context.Context, key, value string) error {
	return c.do(ctx, "project", "set", Project, key+"="+value)
}
