package boxd

import "github.com/a-Fig/agent-box/internal/mask"

// Mask hides what looks like a secret in s (internal/mask, shared with box).
func Mask(s string) string { return mask.Mask(s) }
