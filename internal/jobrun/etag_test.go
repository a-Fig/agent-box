package jobrun

import (
	"context"
	"errors"
	"testing"
)

func TestRetryETag(t *testing.T) {
	race := errors.New("incus config: Error: ETag doesn't match: 1 vs 2")
	busy := errors.New(`incus delete: Error: Failed deleting instance pc:w in project "agents": Failed to create instance delete operation: Instance is busy running a "update" operation`)
	other := errors.New("incus config: Error: Instance not found")
	for name, tc := range map[string]struct {
		errs  []error // what each try returns; nil once they run out
		tries int
		want  error
	}{
		"first try":       {nil, 1, nil},
		"after two lost":  {[]error{race, race}, 3, nil},
		"other error":     {[]error{other}, 1, other},
		"busy, then free": {[]error{busy}, 2, nil},
		"race throughout": {[]error{race, race, race, race, race, race}, 5, race},
	} {
		tries := 0
		err := RetryETag(context.Background(), func() error {
			tries++
			if tries <= len(tc.errs) {
				return tc.errs[tries-1]
			}
			return nil
		})
		if !errors.Is(err, tc.want) || tries != tc.tries {
			t.Errorf("%s: %v after %d tries", name, err, tries)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RetryETag(ctx, func() error { return race }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
