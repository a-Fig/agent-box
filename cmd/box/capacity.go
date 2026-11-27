package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/a-Fig/agent-box/internal/mode"
)

type capacityDetail struct {
	Limits      []string `json:"limits"`
	SmallerSize string   `json:"smallerSize,omitempty"`
}

func launchLimits(err error) []string {
	var room *errNoRoom
	var busy *errGPUBusy
	var noGPU errNoGPU
	switch {
	case errors.As(err, &room):
		return []string{room.resource}
	case errors.As(err, &busy), errors.As(err, &noGPU):
		return []string{"gpu"}
	}
	return nil
}

// capacityHint refreshes the selected host's capacity after the refusal.
// GPU has one size, and a named box would change if the retry resized it.
func (a *App) capacityHint(ctx context.Context, size mode.Size, box string, limits []string) *capacityDetail {
	if len(limits) == 0 {
		return nil
	}
	detail := &capacityDetail{Limits: limits}
	if size.GPU || box != "" || size.Name == mode.Sizes[0].Name {
		return detail
	}
	ctx, cancel := context.WithTimeout(ctx, refusalTimeout)
	defer cancel()
	s := a.probeHostStatus(ctx, hostProbeOpts{strictCaps: true}).report
	if ctx.Err() != nil || !s.Reachable || s.Error != "" || !startableMode(s.Mode) {
		return detail
	}
	for _, smaller := range mode.Sizes {
		if smaller.Name == size.Name {
			break
		}
		if smaller.GPU == size.GPU && sizeFits(s.Sizes, smaller.Name) {
			detail.SmallerSize = smaller.Name
		}
	}
	return detail
}

func (a *App) printCapacity(detail *capacityDetail) {
	if detail == nil {
		return
	}
	facts := strings.NewReplacer("cpu", "CPU", "gpu", "GPU").Replace(strings.Join(detail.Limits, ", "))
	fmt.Fprintf(a.Err, "box: capacity limit%s: %s.", onHost(a.jobHostName), facts)
	if detail.SmallerSize != "" {
		fmt.Fprintf(a.Err, " A %s box appears to fit now if it meets the job's needs.", detail.SmallerSize)
	}
	fmt.Fprintln(a.Err)
}
