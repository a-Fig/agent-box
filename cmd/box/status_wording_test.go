package main

import (
	"strings"
	"testing"
)

// The free count includes boxes a launch replaces, and the line says so (#82).
func TestStatusSaysFreeCountsReplaceableBoxes(t *testing.T) {
	var b strings.Builder
	writeStatus(&b, StatusReport{Advice: "available", Reachable: true, Mode: "full", Boxes: 4, MaxBoxes: 10, SlotsFree: 8, Replaceable: 2})
	if want := "boxes: 4 of 10 (8 free, 2 of them taken by finished throwaways, idle repo boxes or expired boxes a launch replaces)"; !strings.Contains(b.String(), want) {
		t.Fatalf("got:\n%s", b.String())
	}
	b.Reset()
	writeStatus(&b, StatusReport{Advice: "available", Reachable: true, Mode: "full", Boxes: 4, MaxBoxes: 10, SlotsFree: 6})
	if !strings.Contains(b.String(), "boxes: 4 of 10 (6 free)\n") {
		t.Fatalf("got:\n%s", b.String())
	}
}
