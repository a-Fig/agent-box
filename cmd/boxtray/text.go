package main

import (
	"fmt"

	"github.com/a-Fig/agent-box/internal/mode"
)

// statusText is the one-line summary for the tooltip and the menu header.
func statusText(st mode.Status) string {
	switch {
	case st.Down:
		return fmt.Sprintf("%s chosen, WSL not running", st.Chosen.Label())
	case st.Yielding:
		return fmt.Sprintf("Paused for game (%s), %s chosen", st.Reason, st.Chosen.Label())
	case st.Err != nil:
		return fmt.Sprintf("%s, retrying after an error", st.Effective.Label())
	}
	return fmt.Sprintf("%s, %d running", st.Effective.Label(), st.Running)
}
