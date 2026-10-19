package model_test

import (
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine/enginetest"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine/model"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/events"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

type harness struct{ m *model.Engine }

func (h harness) Engine() engine.Engine { return h.m }

func (h harness) Events(_ *testing.T, transactionID string) []events.Envelope {
	return h.m.Events(transactionID)
}

func (h harness) Transaction(t *testing.T, id string) wager.Transaction {
	t.Helper()
	tx, ok := h.m.Transaction(id)
	if !ok {
		t.Fatalf("transaction %s not recorded", id)
	}
	return tx
}

func (h harness) Wallet(t *testing.T, id string) (money.Money, int64) {
	t.Helper()
	balance, version, ok := h.m.WalletBalance(id)
	if !ok {
		t.Fatalf("wallet %s not recorded", id)
	}
	return balance, version
}

func (h harness) LedgerSum(_ *testing.T, walletID string) int64 { return h.m.LedgerSum(walletID) }

func (h harness) LedgerCount(_ *testing.T, transactionID string) int {
	return h.m.LedgerCount(transactionID)
}

func TestModelHonoursTheEngineContract(t *testing.T) {
	enginetest.Run(t, harness{m: model.New()})
}
