package hostport_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/hostport"
)

func TestPublishPinsTheContainerPortToAPortFreeHere(t *testing.T) {
	var req testcontainers.GenericContainerRequest
	if err := hostport.Publish("5432/tcp")(&req); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if req.HostConfigModifier == nil {
		t.Fatal("Publish did not set a host config modifier")
	}

	var hostConfig container.HostConfig
	req.HostConfigModifier(&hostConfig)

	bindings := hostConfig.PortBindings[network.MustParsePort("5432/tcp")]
	if len(bindings) != 1 || bindings[0].HostPort == "" || bindings[0].HostPort == "0" {
		t.Fatalf("bindings = %v, want one fixed host port", bindings)
	}

	// The point of pinning: nothing in this network namespace may already
	// hold the port, or test traffic would reach that process instead.
	listener, err := net.Listen("tcp", "127.0.0.1:"+bindings[0].HostPort)
	if err != nil {
		t.Fatalf("pinned port %s is not free here: %v", bindings[0].HostPort, err)
	}
	_ = listener.Close()
}

// testcontainers drops bindings for ports a module does not expose, so this
// checks the pin survives all the way to Docker.
func TestStartedContainerIsPublishedOnThePinnedPort(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := testcontainers.Run(ctx, "postgres:17-alpine",
		testcontainers.WithEnv(map[string]string{"POSTGRES_PASSWORD": "test"}),
		testcontainers.WithExposedPorts("5432/tcp"),
		hostport.Publish("5432/tcp"),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start container: %v", err)
	}

	inspect, err := ctr.Inspect(ctx)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	pinned := inspect.HostConfig.PortBindings[network.MustParsePort("5432/tcp")]
	if len(pinned) != 1 || pinned[0].HostPort == "" || pinned[0].HostPort == "0" {
		t.Fatalf("Docker received bindings %v, want one fixed host port", pinned)
	}

	mapped, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	if got := strconv.Itoa(int(mapped.Num())); got != pinned[0].HostPort {
		t.Fatalf("container published on %s, want the pinned %s", got, pinned[0].HostPort)
	}
}
