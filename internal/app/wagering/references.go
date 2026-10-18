package wagering

import "context"

// ReferenceClaim is a PENDING_REFERENCE reversal claimed for one retry,
// with the attempts it has had so far.
type ReferenceClaim struct {
	TransactionID string
	Attempts      int
}

// ReferenceQueue hands out the pending reversals whose next attempt is due.
// A claim is a lease: no other caller gets the same reversal until it
// expires, so a crashed worker's claims come back on their own.
type ReferenceQueue interface {
	ClaimDue(ctx context.Context, limit int) ([]ReferenceClaim, error)
}
