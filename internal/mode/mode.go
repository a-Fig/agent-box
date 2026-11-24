// Package mode applies agent-box's three sharing modes (My PC / Shared / Full)
// to the Incus instances in the "agents" project. It knows nothing about
// Windows: it talks to Incus through the Incus interface, so it is tested with
// fakes and builds everywhere.
package mode

// Mode is how much of the PC agents get.
type Mode string

const (
	MyPC   Mode = "mypc"   // every instance paused; long-frozen ones stopped
	Shared Mode = "shared" // half the CPU, normal priority, at most 2 instances
	Full   Mode = "full"   // all the CPU, higher priority, at most 6 instances
)

// All lists the modes in menu order.
var All = []Mode{MyPC, Shared, Full}

// ProjectModeKey is the Incus project key `box` reads to learn the mode.
const ProjectModeKey = "user.agent-box.mode"

// Parse turns a stored string into a Mode.
func Parse(s string) (Mode, bool) {
	for _, m := range All {
		if string(m) == s {
			return m, true
		}
	}
	return "", false
}

// Label is the human name shown in the tray.
func (m Mode) Label() string {
	switch m {
	case MyPC:
		return "My PC"
	case Shared:
		return "Shared"
	case Full:
		return "Full"
	}
	return string(m)
}

// limits are the Incus settings a running mode (Shared, Full) applies.
type limits struct {
	allowance    string // limits.cpu.allowance; "" means unset
	priority     string // limits.cpu.priority
	maxInstances string // project limits.instances
}

func (m Mode) limits() limits {
	if m == Full {
		return limits{allowance: "", priority: "10", maxInstances: "6"}
	}
	return limits{allowance: "50%", priority: "0", maxInstances: "2"}
}
