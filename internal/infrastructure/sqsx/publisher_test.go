package sqsx

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		-1:  2 * time.Second,
		0:   2 * time.Second,
		1:   4 * time.Second,
		5:   64 * time.Second,
		6:   2 * time.Minute,
		100: 2 * time.Minute,
	} {
		if got := backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}
