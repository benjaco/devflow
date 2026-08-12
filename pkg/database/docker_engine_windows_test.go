//go:build windows

package database

import (
	"strings"
	"testing"

	mobyclient "github.com/moby/moby/client"
)

func TestDefaultDockerAPIClientUsesNativeWindowsNamedPipe(t *testing.T) {
	clearDockerEndpointEnvironment(t)
	client, _, err := newDockerAPIClientAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := client.DaemonHost(); got != mobyclient.DefaultDockerHost {
		t.Fatalf("default Windows Docker host = %q, want %q", got, mobyclient.DefaultDockerHost)
	}
	if !strings.HasPrefix(client.DaemonHost(), "npipe://") {
		t.Fatalf("default Windows Docker transport must be a named pipe, got %q", client.DaemonHost())
	}
}
