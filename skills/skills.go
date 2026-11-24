// Package skills holds the agent skills that box join installs.
package skills

import _ "embed"

// Box is skills/box/SKILL.md.
//
//go:embed box/SKILL.md
var Box string
