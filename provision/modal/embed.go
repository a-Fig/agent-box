// Package modalrunner embeds the Modal runner's deployable source, so
// box setup modal can deploy it without a checkout of this repository.
package modalrunner

import "embed"

// FS holds the files modal deploy needs: the app and the two modules it
// imports. Tests, the smoke script and the README stay out.
//
//go:embed agent_box_runner.py core.py rates.py
var FS embed.FS
