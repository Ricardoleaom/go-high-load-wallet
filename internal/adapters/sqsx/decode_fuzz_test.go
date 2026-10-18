package sqsx

import (
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

// Whatever lands on the queue, decoding either refuses it or yields a
// message ID and a submission whose money is well formed; one that also
// passes validation obeys the rules of its kind.
func FuzzDecodeMessage(f *testing.F) {
	for _, seed := range []string{
		`{"messageId":"m1","type":"WagerTransaction","data":{"providerId":"p","externalTransactionId":"e1","idempotencyKey":"k1","playerId":"0198f6a0-0000-7000-8000-000000000001","walletId":"0198f6a0-0000-7000-8000-000000000002","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"10.00","currency":"BRL"}}}`,
		`{"messageId":"m2","data":{"kind":"ROLLBACK","money":{"amount":"1.00","currency":"BRL"},"referenceExternalTransactionId":"e1"}}`,
		`{"messageId":"","data":{}}`,
		`{"data":{"money":{"amount":"abc","currency":"BRL"}}}`,
		`not json`, ``, `{}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		messageID, in, err := decodeMessage(body)
		if err != nil {
			return
		}
		if messageID == "" || in.CorrelationID != messageID {
			t.Fatalf("accepted message ID %q correlation %q", messageID, in.CorrelationID)
		}
		if in.Amount.Currency() == "" || in.Amount.AmountInMinorUnits() < 0 {
			t.Fatalf("accepted amount %s", in.Amount)
		}
		if wagering.Validate(in) != nil {
			return
		}
		units := in.Amount.AmountInMinorUnits()
		switch in.Kind {
		case wager.KindBet, wager.KindWin, wager.KindRefund, wager.KindRollback:
			if units <= 0 {
				t.Fatalf("valid %s of %d", in.Kind, units)
			}
		case wager.KindLoss:
			if units != 0 {
				t.Fatalf("valid LOSS of %d", units)
			}
		default:
			t.Fatalf("valid submission of kind %q", in.Kind)
		}
	})
}
