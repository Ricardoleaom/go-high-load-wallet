package wagering_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine/model"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func bet(walletID, playerID string, amount money.Money) wagering.SubmitInput {
	external := "ext-" + uuid.NewString()
	return wagering.SubmitInput{
		ProviderID: "provider-a", ExternalID: external, IdempotencyKey: "key-" + external,
		PlayerID: playerID, WalletID: walletID, RoundID: "round-1", GameID: "game-1",
		Kind: wager.KindBet, Amount: amount,
	}
}

// newService opens one wallet on a fresh model engine.
func newService(t *testing.T, balance string) (*wagering.Service, string, string) {
	t.Helper()
	service := wagering.NewService(model.New(), nil)
	playerID := uuid.NewString()
	w, err := service.OpenWallet(context.Background(), playerID, brl(t, balance))
	if err != nil {
		t.Fatal(err)
	}
	return service, w.ID(), playerID
}

// refusingEngine fails the test if any command reaches it.
type refusingEngine struct{ t *testing.T }

func (e refusingEngine) Execute(_ context.Context, cmd engine.Command) (engine.Outcome, error) {
	e.t.Errorf("an invalid request reached the engine: %+v", cmd)
	return engine.Outcome{}, errors.New("refused")
}

func TestSubmitRejectsOpening(t *testing.T) {
	service := wagering.NewService(refusingEngine{t}, nil)
	in := bet(uuid.NewString(), uuid.NewString(), brl(t, "10.00"))
	in.Kind = wager.KindOpening
	if _, err := service.Submit(context.Background(), in); !errors.Is(err, wagering.ErrOpeningNotAllowed) {
		t.Fatalf("err = %v, want ErrOpeningNotAllowed", err)
	}
}

func TestSubmitRejectsMalformedIDsAndLongFields(t *testing.T) {
	service := wagering.NewService(refusingEngine{t}, nil)
	long := strings.Repeat("x", wagering.MaxFieldLength+1)

	for _, tc := range []struct {
		name   string
		mutate func(*wagering.SubmitInput)
		want   error
	}{
		{"wallet ID", func(in *wagering.SubmitInput) { in.WalletID = "abc" }, wagering.ErrInvalidID},
		{"player ID", func(in *wagering.SubmitInput) { in.PlayerID = "abc" }, wagering.ErrInvalidID},
		{"provider ID", func(in *wagering.SubmitInput) { in.ProviderID = long }, wagering.ErrFieldTooLong},
		{"external ID", func(in *wagering.SubmitInput) { in.ExternalID = long }, wagering.ErrFieldTooLong},
		{"idempotency key", func(in *wagering.SubmitInput) { in.IdempotencyKey = long }, wagering.ErrFieldTooLong},
		{"round ID", func(in *wagering.SubmitInput) { in.RoundID = long }, wagering.ErrFieldTooLong},
		{"game ID", func(in *wagering.SubmitInput) { in.GameID = long }, wagering.ErrFieldTooLong},
		{"reference", func(in *wagering.SubmitInput) {
			in.Kind = wager.KindRefund
			in.ReferenceExternalID = long
		}, wagering.ErrFieldTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := bet(uuid.NewString(), uuid.NewString(), brl(t, "10.00"))
			tc.mutate(&in)
			_, err := service.Submit(context.Background(), in)
			if !errors.Is(err, tc.want) || !errors.Is(err, wagering.ErrInvalidInput) {
				t.Fatalf("err = %v, want %v (an ErrInvalidInput)", err, tc.want)
			}
		})
	}
}

// Everything that can be judged from the request alone is judged before
// any storage is touched.
func TestSubmitValidatesBeforeTouchingTheEngine(t *testing.T) {
	service := wagering.NewService(refusingEngine{t}, nil)
	for name, tc := range map[string]struct {
		mutate func(*wagering.SubmitInput)
		want   error
	}{
		"loss with an amount":      {func(in *wagering.SubmitInput) { in.Kind = wager.KindLoss }, wager.ErrInvalidAmount},
		"refund without reference": {func(in *wagering.SubmitInput) { in.Kind = wager.KindRefund }, wager.ErrInvalidAmount},
		"zero bet":                 {func(in *wagering.SubmitInput) { in.Amount = brl(t, "0.00") }, wager.ErrInvalidAmount},
		"no round":                 {func(in *wagering.SubmitInput) { in.RoundID = "" }, wager.ErrInvalidTransaction},
		"no idempotency key":       {func(in *wagering.SubmitInput) { in.IdempotencyKey = "" }, wager.ErrInvalidTransaction},
		"unknown kind":             {func(in *wagering.SubmitInput) { in.Kind = "JACKPOT" }, wager.ErrInvalidTransaction},
	} {
		in := bet(uuid.NewString(), uuid.NewString(), brl(t, "10.00"))
		tc.mutate(&in)
		if _, err := service.Submit(context.Background(), in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// UUIDs are case-insensitive: the same wallet in upper case is the same
// wallet, and the same operation is a replay.
func TestSubmitTreatsUpperCaseIDsAsTheSameWallet(t *testing.T) {
	service, walletID, playerID := newService(t, "100.00")

	upper := bet(strings.ToUpper(walletID), strings.ToUpper(playerID), brl(t, "10.00"))
	first, err := service.Submit(context.Background(), upper)
	if err != nil || first.Status != wager.StatusProcessed {
		t.Fatalf("Submit with upper-case IDs = %+v, %v; want PROCESSED", first, err)
	}

	lower := upper
	lower.WalletID, lower.PlayerID = walletID, playerID
	again, err := service.Submit(context.Background(), lower)
	if err != nil || !again.IdempotentReplay || again.TransactionID != first.TransactionID {
		t.Fatalf("same operation in lower case = %+v, %v; want a replay of %s", again, err, first.TransactionID)
	}
}

func TestSubmitReturnsTheEngineOutcome(t *testing.T) {
	service, walletID, playerID := newService(t, "100.00")
	in := bet(walletID, playerID, brl(t, "30.00"))

	first, err := service.Submit(context.Background(), in)
	if err != nil || first.Status != wager.StatusProcessed || first.Balance.String() != "70.00 BRL" || first.IdempotentReplay {
		t.Fatalf("first submission = %+v, %v", first, err)
	}
	replay, err := service.Submit(context.Background(), in)
	if err != nil || !replay.IdempotentReplay || replay.TransactionID != first.TransactionID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	rejected, err := service.Submit(context.Background(), bet(walletID, playerID, brl(t, "500.00")))
	if err != nil || rejected.FailureCode != wager.FailureInsufficientBalance || rejected.Balance.Currency() != "" {
		t.Fatalf("rejection = %+v, %v", rejected, err)
	}

	changed := in
	changed.Amount = brl(t, "31.00")
	if _, err := service.Submit(context.Background(), changed); !errors.Is(err, wagering.ErrIdempotencyConflict) {
		t.Errorf("same key, other payload: err = %v", err)
	}
	if _, err := service.Submit(context.Background(), bet(uuid.NewString(), playerID, brl(t, "1.00"))); !errors.Is(err, wagering.ErrWalletNotFound) {
		t.Errorf("unknown wallet: err = %v", err)
	}
}

// racingEngine answers every submission as a replay that lost a race.
type racingEngine struct{}

func (racingEngine) Execute(context.Context, engine.Command) (engine.Outcome, error) {
	return engine.Outcome{TransactionID: "winner", Status: wager.StatusProcessed, Replay: true, Raced: true}, nil
}

func TestSubmitReportsRacesToTheHook(t *testing.T) {
	service := wagering.NewService(racingEngine{}, nil)
	races := 0
	service.OnConcurrencyConflict(func() { races++ })
	out, err := service.Submit(context.Background(), bet(uuid.NewString(), uuid.NewString(), brl(t, "1.00")))
	if err != nil || !out.IdempotentReplay || races != 1 {
		t.Fatalf("raced submission = %+v, %v; hook called %d times", out, err, races)
	}
}

func TestOpenWalletUsesTheEngine(t *testing.T) {
	service := wagering.NewService(model.New(), nil)
	playerID := uuid.NewString()

	w, err := service.OpenWallet(context.Background(), strings.ToUpper(playerID), brl(t, "25.00"))
	if err != nil {
		t.Fatal(err)
	}
	if w.PlayerID() != playerID || w.Balance().String() != "25.00 BRL" || w.Version() != 1 {
		t.Errorf("opened %s %s v%d", w.PlayerID(), w.Balance(), w.Version())
	}
	if _, err := service.OpenWallet(context.Background(), playerID, brl(t, "1.00")); !errors.Is(err, wagering.ErrWalletConflict) {
		t.Errorf("second wallet in the same currency: err = %v", err)
	}
	if _, err := service.OpenWallet(context.Background(), "not-a-uuid", brl(t, "1.00")); !errors.Is(err, wagering.ErrInvalidID) {
		t.Errorf("malformed player ID: err = %v", err)
	}
}

// fakeQueue hands out a fixed list of claims once.
type fakeQueue struct{ claims []wagering.ReferenceClaim }

func (q *fakeQueue) ClaimDue(context.Context, int) ([]wagering.ReferenceClaim, error) {
	claims := q.claims
	q.claims = nil
	return claims, nil
}

// flakyEngine fails RetryReference for one transaction and records the rest.
type flakyEngine struct {
	mu      sync.Mutex
	failing string
	retried []engine.RetryReference
}

func (e *flakyEngine) Execute(_ context.Context, cmd engine.Command) (engine.Outcome, error) {
	retry := cmd.(engine.RetryReference)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.retried = append(e.retried, retry)
	if retry.TransactionID == e.failing {
		return engine.Outcome{}, errors.New("database unavailable")
	}
	return engine.Outcome{TransactionID: retry.TransactionID, Status: wager.StatusPendingReference}, nil
}

func TestRetryPendingReferencesContinuesPastAFailure(t *testing.T) {
	e := &flakyEngine{failing: "second"}
	queue := &fakeQueue{claims: []wagering.ReferenceClaim{{"first", 1}, {"second", 2}, {"third", 3}}}
	service := wagering.NewService(e, queue)

	handled, err := service.RetryPendingReferences(context.Background(), 10)
	if handled != 2 || err == nil || !strings.Contains(err.Error(), "second") {
		t.Fatalf("RetryPendingReferences = %d, %v; want 2 and an error naming the second", handled, err)
	}
	want := fmt.Sprint([]engine.RetryReference{{TransactionID: "first", Attempts: 1}, {TransactionID: "second", Attempts: 2}, {TransactionID: "third", Attempts: 3}})
	if got := fmt.Sprint(e.retried); got != want {
		t.Errorf("retried %s, want %s", got, want)
	}
}

func TestRetryWorkerRetriesOnEveryTickUntilStopped(t *testing.T) {
	e := &flakyEngine{}
	queue := &fakeQueue{claims: []wagering.ReferenceClaim{{"first", 1}, {"second", 1}}}
	retried := make(chan int, 10)
	worker := wagering.NewRetryWorker(wagering.NewService(e, queue), func(n int) { retried <- n })

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error)
	go func() { stopped <- worker.Run(ctx, 10*time.Millisecond) }()
	select {
	case n := <-retried:
		if n != 2 {
			t.Errorf("first round retried %d, want 2", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never retried")
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Errorf("Run returned %v", err)
	}
}

func TestPayloadHashIgnoresTransport(t *testing.T) {
	in := bet(uuid.NewString(), uuid.NewString(), brl(t, "10.00"))
	base, err := wagering.ComputePayloadHash(in)
	if err != nil {
		t.Fatal(err)
	}

	transport := in
	transport.IdempotencyKey, transport.CorrelationID = "another-key", "another-correlation"
	if got, _ := wagering.ComputePayloadHash(transport); got != base {
		t.Error("the idempotency key or correlation ID changed the payload hash")
	}
	business := in
	business.Amount = brl(t, "10.01")
	if got, _ := wagering.ComputePayloadHash(business); got == base {
		t.Error("another amount kept the same payload hash")
	}
}

func TestMetricLabelsBoundUnknownKinds(t *testing.T) {
	processed := wagering.Result{Status: wager.StatusProcessed}
	for _, tc := range []struct {
		kind               wager.Kind
		result             wagering.Result
		err                error
		wantKind, wantStat string
	}{
		{wager.KindBet, processed, nil, "BET", "PROCESSED"},
		{"JACKPOT", processed, nil, "OTHER", "PROCESSED"},
		{wager.KindWin, wagering.Result{}, errors.New("boom"), "WIN", "error"},
	} {
		kind, status := wagering.MetricLabels(tc.kind, tc.result, tc.err)
		if kind != tc.wantKind || status != tc.wantStat {
			t.Errorf("MetricLabels(%s) = %s %s, want %s %s", tc.kind, kind, status, tc.wantKind, tc.wantStat)
		}
	}
}
