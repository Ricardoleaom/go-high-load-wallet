// Package hostport pins the host side of a test container's published port.
//
// Docker Desktop on WSL allocates published ports inside its own VM, blind to
// what listens in the WSL distro, and will publish a port a local process
// already holds on 127.0.0.1: the test then talks to that process instead of
// the container. Picking a port nothing here listens on first rules that out.
//
// A small window remains: the probe releases the port before Docker
// publishes it, so a process that binds that exact port in between would
// still be reached instead of the container. Ephemeral ports make the
// collision unlikely, and a test that hits it fails rather than passes.
package hostport

import (
	"fmt"
	"net"
	"strconv"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
)

// Publish binds containerPort (such as "5432/tcp") to a host port that
// nothing in this network namespace is listening on.
func Publish(containerPort string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		port, err := network.ParsePort(containerPort)
		if err != nil {
			return fmt.Errorf("parse container port %q: %w", containerPort, err)
		}

		hostPort, err := free()
		if err != nil {
			return err
		}

		return testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
			if hostConfig.PortBindings == nil {
				hostConfig.PortBindings = network.PortMap{}
			}
			hostConfig.PortBindings[port] = []network.PortBinding{{HostPort: hostPort}}
		})(req)
	}
}

func free() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve a local port: %w", err)
	}
	defer func() { _ = listener.Close() }()

	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), nil
}
