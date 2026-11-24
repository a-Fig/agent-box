package mode

import (
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
}

// Sizes is the size table, smallest first. docs/DESIGN.md and the box skill
// quote it.
var Sizes = []Size{
	{Name: "small", SharedCPUs: 1, FullCPUs: 2, Memory: "2GiB", Disk: "20GiB"},
	{Name: "medium", SharedCPUs: 2, FullCPUs: 4, Memory: "4GiB", Disk: "30GiB"},
	{Name: "large", SharedCPUs: 4, FullCPUs: 8, Memory: "8GiB", Disk: "40GiB"},
}

// SizeKey is the instance config key that holds a box's size.
const SizeKey = "user.box.size"

// DefaultSize is the size of a box without SizeKey, and of a new box when
// none is asked for.
const DefaultSize = "medium"

// SizeNames is "small, medium, large", for help text and errors.
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
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			v, mult = strings.TrimSuffix(v, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}
