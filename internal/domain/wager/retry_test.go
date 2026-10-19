package wager

import (
	"testing"
	"time"
)

func TestReferenceBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{7, 5 * time.Minute},
		{100, 5 * time.Minute},
	}
	for _, tt := range tests {
		if got := ReferenceBackoff(tt.attempt); got != tt.want {
			t.Errorf("ReferenceBackoff(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
	if MaxReferenceAttempts != 8 {
		t.Errorf("MaxReferenceAttempts = %d, want 8", MaxReferenceAttempts)
	}
}
