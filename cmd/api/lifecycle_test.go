package main

import (
	"net/http"
	"testing"
	"time"
)

func TestNewServerSetsTimeouts(t *testing.T) {
	server := newServer(":0", http.NotFoundHandler(), 7*time.Second)
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
