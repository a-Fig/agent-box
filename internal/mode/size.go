package mode

import (
	"math"
	"strconv"
	"strings"
)

// Size is one box size. A box records its size in SizeKey when `box`
// launches it; memory and root disk are set on the instance then, and the
// tray sets limits.cpu from the size and the mode on every tick.
type Size struct {
	Name       string `json:"name"`
	SharedCPUs int    `json:"sharedCpus"`
	FullCPUs   int    `json:"fullCpus"`
	Memory     string `json:"memory"` // limits.memory
	Disk       string `json:"disk"`   // root disk size
	// GPU boxes get the PC's RTX 3070 (GPUDevices, GPUEnv). One runs at a
	// time, always as a throwaway, and the tray stops it rather than pausing
	// it, because a frozen CUDA process keeps its VRAM.
	GPU bool `json:"gpu,omitempty"`
}

// Sizes is the size table, smallest first. docs/DESIGN.md and the box skill
// quote it.
var Sizes = []Size{
	{Name: "small", SharedCPUs: 1, FullCPUs: 2, Memory: "2GiB", Disk: "18GiB"},
	{Name: "medium", SharedCPUs: 2, FullCPUs: 4, Memory: "4GiB", Disk: "30GiB"},
	{Name: "large", SharedCPUs: 4, FullCPUs: 8, Memory: "8GiB", Disk: "40GiB"},
	{Name: GPUSize, SharedCPUs: 4, FullCPUs: 6, Memory: "8GiB", Disk: "40GiB", GPU: true},
}

// GPUSize is the name of the GPU box size.
const GPUSize = "gpu"

// GPUDevices are the Incus devices that give a box the GPU under WSL2,
// proven by hand on the PC on 2026-11-24: WSL exposes the GPU as /dev/dxg,
// its user-space libraries (libcuda, libnvidia-ml, nvidia-smi) in
// /usr/lib/wsl/lib, and Windows' driver store in /usr/lib/wsl/drivers,
// without which nvidia-smi can't reach the driver. The project must allow
// unix-char devices and these two host paths (provision/setup-distro.sh).
var GPUDevices = map[string]map[string]string{
	"dxg":        {"type": "unix-char", "path": "/dev/dxg", "mode": "0666"},
	"wsllib":     {"type": "disk", "source": "/usr/lib/wsl/lib", "path": "/usr/lib/wsl/lib", "readonly": "true"},
	"wsldrivers": {"type": "disk", "source": "/usr/lib/wsl/drivers", "path": "/usr/lib/wsl/drivers", "readonly": "true"},
}

// GPUEnv is the instance config a GPU box gets beside GPUDevices, so CUDA
// programs find WSL's libcuda.
var GPUEnv = map[string]string{"environment.LD_LIBRARY_PATH": "/usr/lib/wsl/lib"}

// StoppedKey is the instance key the tray sets on a GPU box just before it
// stops it, saying why ("mypc", or the yield reason like "gaming: steam:570"),
// so `box` can tell the agent its job was killed on purpose.
const StoppedKey = "user.box.stopped"

// IsGPU says whether an instance's config records the GPU size.
func IsGPU(config map[string]string) bool { return config[SizeKey] == GPUSize }

// SizeKey is the instance config key that holds a box's size.
const SizeKey = "user.box.size"

// DefaultSize is the size of a box without SizeKey, and of a new box when
// none is asked for.
const DefaultSize = "medium"

// SizeNames is "small, medium, large, gpu", for help text and errors.
func SizeNames() string {
	n := make([]string, len(Sizes))
	for i, s := range Sizes {
		n[i] = s.Name
	}
	return strings.Join(n, ", ")
}

// LookupSize finds a size by name.
func LookupSize(name string) (Size, bool) {
	for _, s := range Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return Size{}, false
}

// SizeOf is the size an instance's config records; medium when the key is
// missing or unknown.
func SizeOf(config map[string]string) Size {
	if s, ok := LookupSize(config[SizeKey]); ok {
		return s
	}
	s, _ := LookupSize(DefaultSize)
	return s
}

// CPUs is how many CPUs a box of this size may use in mode m: 0 when boxes
// are paused (My PC) or the mode is unknown.
func (s Size) CPUs(m Mode) int {
	switch m {
	case Shared:
		return s.SharedCPUs
	case Full:
		return s.FullCPUs
	}
	return 0
}

// ParseBytes reads an Incus size like "4GiB", "512MB" or "1073741824".
func ParseBytes(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"PiB", 1 << 50}, {"EiB", 1 << 60},
		{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"PB", 1e15}, {"EB", 1e18}, {"B", 1}, {" bytes", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			v, mult = strings.TrimSuffix(v, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, false
	}
	return n * mult, true
}
