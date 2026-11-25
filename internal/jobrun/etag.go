package jobrun

import (
	"context"
	"strings"
	"time"
)

// RetryETag runs set, a single `incus config set` or `incus project set`,
// again while it loses a write race. Those commands read the config and
// write it back with its ETag, and the tray writes the project's keys and
// each box's CPU limits every few seconds when they differ from what it
// wants, so a write can meet 412 "ETag doesn't match". A 412 means nothing
// was written, so a retry is safe. It tries five times, 200ms, 400ms, ...
// apart, and returns ctx.Err() if ctx ends while it waits.
func RetryETag(ctx context.Context, set func() error) error {
	var err error
	for try := 0; try < 5; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(try) * 200 * time.Millisecond):
			}
		}
		if err = set(); err == nil || !IsETagRace(err) {
			return err
		}
	}
	return err
}

// IsETagRace says whether err is Incus refusing a write whose ETag is stale.
func IsETagRace(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ETag doesn't match")
}
