package gofail

import "testing"

func TestPass(t *testing.T) { t.Log("passing output is hidden") }

func TestSkip(t *testing.T) { t.Skip("not here") }

func TestAssert(t *testing.T) {
	got := 3
	if got != 2 {
		t.Errorf("queue length: expected 2 got %d\n  extra detail line", got)
	}
}

func TestSub(t *testing.T) {
	t.Run("ok", func(t *testing.T) {})
	t.Run("bad", func(t *testing.T) { t.Fatal("subtest broke") })
}

func TestPanic(t *testing.T) {
	var m map[string]int
	m["x"] = 1
}
