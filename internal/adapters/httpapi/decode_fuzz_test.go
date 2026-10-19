package httpapi

import (
	"bytes"
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ids"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

// Whatever a client sends, decoding answers with a 4xx or a submission,
// and a submission that also passes validation obeys every rule of its
// kind: the engine never sees a request it would have to reject itself.
func FuzzDecodeSubmitRequest(f *testing.F) {
	for _, seed := range []string{
		`{"externalTransactionId":"e1","playerId":"0198f6a0-0000-7000-8000-000000000001","walletId":"0198f6a0-0000-7000-8000-000000000002","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"10.00","currency":"BRL"}}`,
		`{"externalTransactionId":"e2","playerId":"0198f6a0-0000-7000-8000-000000000001","walletId":"0198f6a0-0000-7000-8000-000000000002","roundId":"r","gameId":"g","kind":"REFUND","money":{"amount":"10.00","currency":"BRL"},"referenceExternalTransactionId":"e1"}`,
		`{"kind":"LOSS","money":{"amount":"0.00","currency":"BRL"}}`,
		`{"providerId":"provider-b","kind":"BET"}`,
		`{"kind":"OPENING","money":{"amount":"1.00","currency":"BRL"}}`,
		`{"money":{"amount":"-1.00","currency":"brl"}}`,
		`{"money":{"amount":"99999999999999999999.00","currency":"BRL"}}`,
		`[]`, `null`, ``, `{`,
	} {
		f.Add([]byte(seed), "key-1")
	}

	f.Fuzz(func(t *testing.T, body []byte, key string) {
		in, apiErr := decodeSubmitRequest(bytes.NewReader(body), "provider-a", key)
		if apiErr != nil {
			if apiErr.status < 400 || apiErr.status > 499 || apiErr.code == "" {
				t.Fatalf("decoding answered %d %q", apiErr.status, apiErr.code)
			}
			return
		}
		if in.ProviderID != "provider-a" || in.IdempotencyKey != key || key == "" {
			t.Fatalf("accepted provider %q key %q", in.ProviderID, in.IdempotencyKey)
		}
		if in.Kind == wager.KindOpening || in.Amount.Currency() == "" || in.Amount.AmountInMinorUnits() < 0 {
			t.Fatalf("accepted kind %s amount %s", in.Kind, in.Amount)
		}
		if wagering.Validate(in) != nil {
			return
		}
		requireRulesOfItsKind(t, in)
	})
}

func requireRulesOfItsKind(t *testing.T, in wagering.SubmitInput) {
	t.Helper()
	if !ids.Valid(in.WalletID) || !ids.Valid(in.PlayerID) {
		t.Fatalf("valid submission with IDs %q %q", in.WalletID, in.PlayerID)
	}
	units := in.Amount.AmountInMinorUnits()
	switch in.Kind {
	case wager.KindBet, wager.KindWin:
		if units <= 0 {
			t.Fatalf("valid %s of %d", in.Kind, units)
		}
	case wager.KindLoss:
		if units != 0 {
			t.Fatalf("valid LOSS of %d", units)
		}
	case wager.KindRefund, wager.KindRollback:
		if units <= 0 || in.ReferenceExternalID == "" {
			t.Fatalf("valid %s of %d referencing %q", in.Kind, units, in.ReferenceExternalID)
		}
	default:
		t.Fatalf("valid submission of kind %q", in.Kind)
	}
}
