package dash

import "time"

// UpdateInfo is the latest stable release boxd last learned of from GitHub
// (docs/releasing.md). CheckedAt is omitted until a check has succeeded. When
// CheckedAt is set and Latest is "", GitHub reported no stable release yet.
type UpdateInfo struct {
	Latest    string    `json:"latest"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// CodeUpdateCheck is 502 when boxd could not query GitHub for the latest stable release.
const CodeUpdateCheck = "update-check"

const UpdateCheckFailed = "Couldn't check GitHub for updates. Try again."

// EventUpdates is the SSE event carrying a refreshed UpdateInfo after a check.
const EventUpdates = "updates"
