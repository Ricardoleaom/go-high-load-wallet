package lifecycle_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/lifecycle"
)

func TestNewServerSetsTimeouts(t *testing.T) {
	server := lifecycle.NewServer(":0", http.NotFoundHandler(), 7*time.Second)
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", server.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", server.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", server.WriteTimeout, 7 * time.Second},
		{"IdleTimeout", server.IdleTimeout, 120 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestServerServesUntilStopped(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	server := lifecycle.Server("test", lifecycle.NewServer(addr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), time.Second))
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + addr); err == nil {
		t.Error("the server still answers after Stop")
	}
}

// A taken port must fail the start, not a goroutine nobody watches.
func TestServerFailsToStartOnATakenPort(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	server := lifecycle.Server("test", lifecycle.NewServer(taken.Addr().String(), http.NotFoundHandler(), time.Second))
	if err := server.Start(context.Background()); err == nil || !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Fatalf("Start on a taken port = %v, want an error naming it", err)
	}
}

func TestBackgroundStopsItsWorkerAndWaitsForIt(t *testing.T) {
	exited := make(chan struct{})
	worker := lifecycle.Background("worker", func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		close(exited)
		return nil
	})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := worker.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Stop returned before the worker exited")
	}
}

func TestBackgroundStopIsBoundedByItsContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	worker := lifecycle.Background("stuck", func(context.Context) error {
		<-release
		return errors.New("late")
	})
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := worker.Stop(ctx); err == nil || !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("Stop of a stuck worker = %v, want an error naming it", err)
	}
}
