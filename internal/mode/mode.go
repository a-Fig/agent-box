// Package mode applies agent-box's three sharing modes (My PC / Shared / Full)
// to the Incus instances in the "agents" project. It knows nothing about
// Windows: it talks to Incus through the Incus interface, so it is tested with
// fakes and builds everywhere.
package mode

// Mode is how much of the PC agents get.
type Mode string

const (
	MyPC   Mode = "mypc"   // every instance paused; long-frozen ones stopped
	Shared Mode = "shared" // each box may use its size's Shared CPUs, normal priority
	Full   Mode = "full"   // each box may use its size's Full CPUs, higher priority
)

// All lists the modes in menu order.
var All = []Mode{MyPC, Shared, Full}

// ProjectModeKey is the Incus project key `box` reads to learn the mode.
const ProjectModeKey = "user.agent-box.mode"

// ProjectReasonKey says why the tray is yielding (e.g. "gaming: steam:570"); empty otherwise.
const ProjectReasonKey = "user.agent-box.reason"

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

// priority is the limits.cpu.priority a running mode (Shared, Full) sets on
// every box. The CPU count comes from the box's size (Size.CPUs):
// limits.cpu=N is a real cap (N CPUs), unlike limits.cpu.allowance, which is
// only a weight between Linux processes and does nothing against Windows.
// Memory stays as the box was launched with. Project limits are fixed at
// provisioning, because Incus refuses to lower them below what is in use.
func (m Mode) priority() string {
	if m == Full {
		return "10"
	}
	return "0"
}

// legacyKeys are instance keys earlier versions set; they are removed if found.
var legacyKeys = []string{"limits.cpu.allowance"}
