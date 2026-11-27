package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// GPU has one size. Changing the size for an existing named box could
// send the job to a throwaway instead.
func (a *App) capacityHint(ctx context.Context, size mode.Size, box string, limits []string) *capacityDetail {
	if len(limits) == 0 {
		return nil
	}
	detail := &capacityDetail{Limits: limits}
	if size.GPU || size.Name == mode.Sizes[0].Name {
		return detail
	}
	ctx, cancel := context.WithTimeout(ctx, refusalTimeout)
	defer cancel()
	probe := a.probeHostStatus(ctx, hostProbeOpts{strictCaps: true})
	s := probe.report
	if ctx.Err() != nil || !s.Reachable || s.Error != "" || !startableMode(s.Mode) ||
		(box != "" && find(probe.insts, box) != nil) {
		return detail
	}
	for _, limit := range limits {
		switch limit {
		case "instances", "cpu", "memory":
			// The strict probe read these keys directly.
		case "disk", "containers":
			resource := probe.resources[limit]
			if resource.Limit != nil && *resource.Limit == -1 {
				continue
			}
			if _, known := resource.remaining(); !known {
				return detail
			}
		default:
			// The room estimate does not account for this quota. In
			// particular, aggregate disk room does not verify a pool cap.
			return detail
		}
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
	labels := slices.Clone(detail.Limits)
	for i, name := range labels {
		if name == "cpu" || name == "gpu" {
			labels[i] = strings.ToUpper(name)
		}
	}
	fmt.Fprintf(a.Err, "box: capacity limit%s: %s.", onHost(a.jobHostName), strings.Join(labels, ", "))
	if detail.SmallerSize != "" {
		fmt.Fprintf(a.Err, " A %s box appears to fit now if it meets the job's needs.", detail.SmallerSize)
	}
	fmt.Fprintln(a.Err)
}
