package jobrun

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

// LimitRefusal matches Incus refusing a launch over a project cap
// (limits.memory and the like, or limits.instances).
var LimitRefusal = regexp.MustCompile(`(?i)(?:^|: )reached maximum (?:aggregate value "[^"]+" for "limits\.[^"]+"|number of instances(?: of type "[^"]+")?) in project "`)

// IsDisallowedMetadata reports whether err is the Incus 7.6 (2026-11-24) btrfs
// launch failure where a concurrent config write while EnsureImage unpacks an
// image creates containers/<project>_<inst> as a plain directory before `btrfs
// subvolume snapshot`, nesting the snapshot inside it so GHSA-5pgx-q9xx-j3qv's
// stripInstanceMetadata rejects the launch and leaves that empty directory
// behind. Because that name stays poisoned on disk, only a retry under a fresh
// instance name can recover (used by cmd/box's launchThrowaway; boxd binds the
// box name in the queue and drops a failed start).
func IsDisallowedMetadata(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Instance metadata contained disallowed entries")
}

// EndedKeep is how long a throwaway box and its log outlive a job that
// ended, so box pull and the full log still work after the result was pushed.
// The host sets the expiry from the recorded job end.
const EndedKeep = 30 * time.Minute

// Instance keys a box is launched with (cmd/box reads them too).
const (
	ExpiresKey        = "user.expires"
	ProtectedExpiry   = mode.ProtectedExpiry
	SetupKey          = "user.box.setup"
	SetupTTL          = 10 * time.Minute
	ThrowawayKey      = "user.box.throwaway"
	ThrowawayRepoKey  = "user.box.repo-key"
	ThrowawayOwnerKey = "user.box.pool-job"
	ThrowawayClaimKey = "user.box.claim-until"
	ThrowawayClaimTTL = SetupTTL
	// ThrowawayStopKey holds an instance-bound stop reservation until the
	// stop is confirmed, then ThrowawayStopped identifies the kept disk.
	ThrowawayStopKey = "user.box.idle-stop"
	ThrowawayStopped = "stopped"
	// A client can fence cooling through Incus while boxd is unavailable.
	DirectEvictKey = "user.box.direct-evict-until"
	EvictTTL       = 30 * time.Second
)

func SetupValue(job string, now time.Time) string {
	return job + " " + now.Add(SetupTTL).UTC().Format(time.RFC3339Nano)
}

func SetupOwner(cfg map[string]string) (string, time.Time) {
	job, stamp, _ := strings.Cut(cfg[SetupKey], " ")
	until, _ := time.Parse(time.RFC3339Nano, stamp)
	return job, until
}

func DeleteReserved(cfg map[string]string, now time.Time) bool {
	until, _ := time.Parse(time.RFC3339Nano, cfg[DirectEvictKey])
	return until.After(now)
}

// ThrowawayClaimed reports whether a client or an idle stop reserved the box.
func ThrowawayClaimed(cfg map[string]string, now time.Time) bool {
	until, _ := time.Parse(time.RFC3339, cfg[ThrowawayClaimKey])
	return until.After(now) || DeleteReserved(cfg, now) || IdleStopReserved(cfg)
}

func IdleStopReserved(cfg map[string]string) bool {
	return cfg[ThrowawayStopKey] != "" && cfg[ThrowawayStopKey] != ThrowawayStopped
}

// LaunchFlags are the -c and -d flags of `incus launch` for a box of size sz
// that expires at expires, with extra flags after the size's. The size's memory and root disk are set here,
// once; the tray sets limits.cpu from the size and the mode, so the Shared
// values are only a start (each write the tray makes can race another writer's ETag). A GPU box also gets devices, as JSON (which is
// YAML) for launch's stdin: -d only overrides devices a profile already has.
func LaunchFlags(sz mode.Size, expires time.Time, throwaway bool, extra ...string) (flags []string, devices string) {
	expiry := expires.UTC().Format(time.RFC3339)
	for i := 1; i < len(extra); i++ {
		if extra[i-1] == "-c" && strings.HasPrefix(extra[i], SetupKey+"=") {
			expiry = ProtectedExpiry
			break
		}
	}
	flags = []string{"-c", ExpiresKey + "=" + expiry,
		"-c", mode.SizeKey + "=" + sz.Name,
		"-c", "limits.cpu=" + strconv.Itoa(sz.SharedCPUs),
		"-c", "limits.cpu.priority=0", // Shared's, so the tray has nothing to write then
		"-c", "limits.memory=" + sz.Memory,
		"-d", "root,size=" + sz.Disk}
	if throwaway {
		flags = append(flags, "-c", ThrowawayKey+"=true")
	}
	flags = append(flags, extra...)
	if sz.GPU {
		keys := make([]string, 0, len(mode.GPUEnv))
		for k := range mode.GPUEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			flags = append(flags, "-c", k+"="+mode.GPUEnv[k])
		}
		b, _ := json.Marshal(map[string]any{"devices": mode.GPUDevices})
		devices = string(b)
	}
	return flags, devices
}
