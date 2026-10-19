package wagering

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// canonicalPayload fixes the field order used for hashing, independent of
// the struct field order anywhere else in the codebase. It deliberately
// excludes the idempotency key and any transport metadata, so HTTP and SQS
// produce the same hash for the same business operation.
type canonicalPayload struct {
	Amount                         string `json:"amount"`
	Currency                       string `json:"currency"`
	ExternalID                     string `json:"externalId"`
	GameID                         string `json:"gameId"`
	Kind                           string `json:"kind"`
	PlayerID                       string `json:"playerId"`
	ProviderID                     string `json:"providerId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
	RoundID                        string `json:"roundId"`
	WalletID                       string `json:"walletId"`
}

// ComputePayloadHash returns a deterministic SHA-256 hex digest of the
// business fields of a submission.
func ComputePayloadHash(input SubmitInput) (string, error) {
	payload := canonicalPayload{
		Amount:                         input.Amount.DecimalString(),
		Currency:                       input.Amount.Currency(),
		ExternalID:                     input.ExternalID,
		GameID:                         input.GameID,
		Kind:                           string(input.Kind),
		PlayerID:                       input.PlayerID,
		ProviderID:                     input.ProviderID,
		ReferenceExternalTransactionID: input.ReferenceExternalID,
		RoundID:                        input.RoundID,
		WalletID:                       input.WalletID,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal canonical payload: %w", err)
	}

	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
