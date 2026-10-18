package wager

import "time"

// MaxReferenceAttempts bounds how many times a REFUND or ROLLBACK waits for
// its original before it is rejected with REFERENCE_NOT_FOUND_TIMEOUT.
const MaxReferenceAttempts = 8

// ReferenceBackoff is how long a reversal waits before attempt number
// attempt: 5 seconds, doubling, at most 5 minutes.
func ReferenceBackoff(attempt int) time.Duration {
	const base = 5 * time.Second
	const maxBackoff = 5 * time.Minute
	const maxShift = 20 // base<<20 already dwarfs maxBackoff; caps the shift count

	if attempt <= 0 {
		return base
	}

	shift := attempt - 1
	if shift > maxShift {
		shift = maxShift
	}

	d := base << uint(shift)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}
