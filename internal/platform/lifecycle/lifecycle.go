// Package lifecycle starts and stops the process's long-running parts, the
// HTTP servers and the background workers, with no dependency on how they
// are composed: cmd/ hooks each Component into Fx.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Component is something that runs between Start and Stop.
type Component interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// NewServer bounds how long a client may take to send a request, how long
// a response may take to write and how long an idle keep-alive connection
// is kept, so slow or idle clients cannot hold connections indefinitely.
func NewServer(addr string, handler http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
	}
}

// Server listens on Start, so a taken port fails the start, and drains
// in-flight requests on Stop within the stop context's deadline.
func Server(name string, server *http.Server) Component {
	return &httpServer{name: name, server: server}
}

type httpServer struct {
	name   string
	server *http.Server
}

func (s *httpServer) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.server.Addr, err)
	}
	go func() {
		slog.Info("server listening", "server", s.name, "addr", listener.Addr().String())
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped with error", "server", s.name, "error", err)
		}
	}()
	return nil
}

func (s *httpServer) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// Background runs run in its own goroutine from Start; Stop cancels its
// context and waits, within the stop context's deadline, for it to return,
// so shutdown is observable rather than fire-and-forget.
func Background(name string, run func(ctx context.Context) error) Component {
	return &background{name: name, run: run}
}

type background struct {
	name   string
	run    func(ctx context.Context) error
	cancel context.CancelFunc
	done   chan struct{}
}

func (b *background) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done = make(chan struct{})
	go func() {
		defer close(b.done)
		if err := b.run(ctx); err != nil {
			slog.Error("background worker stopped with error", "worker", b.name, "error", err)
		}
	}()
	return nil
}

func (b *background) Stop(ctx context.Context) error {
	b.cancel()
	select {
	case <-b.done:
		slog.Info("background worker stopped", "worker", b.name)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker %s did not stop before the shutdown deadline", b.name)
	}
}
