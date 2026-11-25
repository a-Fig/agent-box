package jobrun

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

// LimitRefusal matches Incus refusing a launch over a project cap
// (limits.memory and the like, or limits.instances).
var LimitRefusal = regexp.MustCompile(`(?i)(?:^|: )reached maximum (?:aggregate value "[^"]+" for "limits\.[^"]+"|number of instances(?: of type "[^"]+")?) in project "`)

// EndedKeep is how long a throwaway box and its log outlive a job that
// ended, so box pull and the full log still work after the result was pushed.
// box run and boxd both move its expiry to now plus this; the reaper deletes it.
const EndedKeep = 30 * time.Minute

// Instance keys a box is launched with (cmd/box reads them too).
const (
	ExpiresKey   = "user.expires"
	ThrowawayKey = "user.box.throwaway"
)

// LaunchFlags are the -c and -d flags of `incus launch` for a box of size sz
// that expires at expires, with extra flags after the size's. The size's memory and root disk are set here,
// once; the tray sets limits.cpu from the size and the mode, so the Shared
// value is only a start. A GPU box also gets devices, as JSON (which is
// YAML) for launch's stdin: -d only overrides devices a profile already has.
func LaunchFlags(sz mode.Size, expires time.Time, throwaway bool, extra ...string) (flags []string, devices string) {
	flags = []string{"-c", ExpiresKey + "=" + expires.UTC().Format(time.RFC3339),
		"-c", mode.SizeKey + "=" + sz.Name,
		"-c", "limits.cpu=" + strconv.Itoa(sz.SharedCPUs),
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
