//go:build windows

package main

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// Codex's job object refuses breakaway (access denied): the watcher starts
// without it. Any other failure is not retried.
func TestStartDetachedFallsBackWithoutBreakaway(t *testing.T) {
	var tries []bool
	err := startDetached(func(breakaway bool) error {
		tries = append(tries, breakaway)
		if breakaway {
			return fmt.Errorf("fork/exec box.exe: %w", syscall.ERROR_ACCESS_DENIED)
		}
		return nil
	})
	if err != nil || fmt.Sprint(tries) != "[true false]" {
		t.Errorf("denied breakaway: %v, tries %v", err, tries)
	}
	tries = nil
	other := errors.New("fork/exec box.exe: file not found")
	if err := startDetached(func(breakaway bool) error { tries = append(tries, breakaway); return other }); !errors.Is(err, other) || len(tries) != 1 {
		t.Errorf("other failure: %v, tries %v", err, tries)
	}
}
