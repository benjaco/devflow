package database

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	contextdocker "github.com/docker/cli/cli/context/docker"
	contextstore "github.com/docker/cli/cli/context/store"
	"github.com/moby/moby/api/pkg/stdcopy"
	containertypes "github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestDockerAPIClientResolvesCurrentContextWithoutDockerExecutable(t *testing.T) {
	clearDockerEndpointEnvironment(t)
	configDir := t.TempDir()
	writeDockerContext(t, configDir, "desktop-test", "tcp://127.0.0.1:42371")
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"currentContext":"desktop-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	client, _, err := newDockerAPIClientAt(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := client.DaemonHost(); got != "tcp://127.0.0.1:42371" {
		t.Fatalf("context daemon host = %q, want %q", got, "tcp://127.0.0.1:42371")
	}
}

func TestDockerAPIClientEnvironmentContextOverridesConfiguredContext(t *testing.T) {
	clearDockerEndpointEnvironment(t)
	configDir := t.TempDir()
	writeDockerContext(t, configDir, "configured", "tcp://127.0.0.1:42372")
	writeDockerContext(t, configDir, "environment", "tcp://127.0.0.1:42373")
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"currentContext":"configured"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONTEXT", "environment")

	client, _, err := newDockerAPIClientAt(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := client.DaemonHost(); got != "tcp://127.0.0.1:42373" {
		t.Fatalf("environment context daemon host = %q, want %q", got, "tcp://127.0.0.1:42373")
	}
}

func TestDockerAPIClientHostEnvironmentTakesPrecedenceOverContext(t *testing.T) {
	clearDockerEndpointEnvironment(t)
	configDir := t.TempDir()
	writeDockerContext(t, configDir, "configured", "tcp://127.0.0.1:42374")
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"currentContext":"configured"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(mobyclient.EnvOverrideHost, "tcp://127.0.0.1:42375")
	t.Setenv("DOCKER_CONTEXT", "configured")

	client, _, err := newDockerAPIClientAt(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := client.DaemonHost(); got != "tcp://127.0.0.1:42375" {
		t.Fatalf("DOCKER_HOST daemon host = %q, want %q", got, "tcp://127.0.0.1:42375")
	}
}

func TestSDKDockerEngineCreatesAndStartsStructuredDatabaseContainer(t *testing.T) {
	var request containertypes.CreateRequest
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, incoming *http.Request) {
		paths = append(paths, incoming.URL.Path)
		switch {
		case strings.HasSuffix(incoming.URL.Path, "/containers/create"):
			if got := incoming.URL.Query().Get("name"); got != "devflow-pg-test" {
				t.Errorf("container name query = %q", got)
			}
			if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"Id":"container-id","Warnings":[]}`)
		case strings.HasSuffix(incoming.URL.Path, "/containers/container-id/start"):
			response.WriteHeader(http.StatusNoContent)
		default:
			http.Error(response, "unexpected Engine API request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := mobyclient.New(
		mobyclient.WithHost("tcp://"+server.Listener.Addr().String()),
		mobyclient.WithAPIVersion("1.55"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	engine := &sdkDockerEngine{client: client}
	err = engine.RunContainer(context.Background(), dockerContainerSpec{
		Name:              "devflow-pg-test",
		Image:             "postgres:18-bookworm",
		Labels:            map[string]string{"devflow.managed": "true"},
		Env:               []string{"POSTGRES_DB=app"},
		HostPort:          55432,
		ContainerPort:     5432,
		VolumeName:        "devflow-pgdata-test",
		VolumeDestination: postgres18DataMount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/containers/create") || !strings.HasSuffix(paths[1], "/containers/container-id/start") {
		t.Fatalf("unexpected Engine API request sequence: %+v", paths)
	}
	if request.Config == nil || request.Config.Image != "postgres:18-bookworm" || !slices.Contains(request.Config.Env, "POSTGRES_DB=app") {
		t.Fatalf("unexpected portable container config: %+v", request.Config)
	}
	if request.HostConfig == nil || len(request.HostConfig.Mounts) != 1 {
		t.Fatalf("unexpected host config: %+v", request.HostConfig)
	}
	mounted := request.HostConfig.Mounts[0]
	if mounted.Source != "devflow-pgdata-test" || mounted.Target != postgres18DataMount {
		t.Fatalf("unexpected volume mount: %+v", mounted)
	}
	foundPort := false
	for port, bindings := range request.HostConfig.PortBindings {
		if port.Port() == "5432" && len(bindings) == 1 && bindings[0].HostPort == "55432" {
			foundPort = true
		}
	}
	if !foundPort {
		t.Fatalf("expected structured 55432:5432 binding, got %+v", request.HostConfig.PortBindings)
	}
}

func TestSDKDockerEngineExecCancellationClosesHijackedStream(t *testing.T) {
	upgraded := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/containers/devflow-pg-test/exec"):
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"Id":"exec-id"}`)
		case strings.HasSuffix(request.URL.Path, "/exec/exec-id/start"):
			connection, _, err := response.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack exec stream: %v", err)
				return
			}
			defer connection.Close()
			if _, err := fmt.Fprint(connection, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\n"); err != nil {
				t.Errorf("write exec upgrade response: %v", err)
				return
			}
			close(upgraded)
			_, _ = io.Copy(io.Discard, connection)
		default:
			http.Error(response, "unexpected Engine API request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := mobyclient.New(
		mobyclient.WithHost("tcp://"+server.Listener.Addr().String()),
		mobyclient.WithAPIVersion("1.55"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	engine := &sdkDockerEngine{client: client}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := engine.Exec(ctx, "devflow-pg-test", []string{"pg_isready"})
		done <- err
	}()
	select {
	case <-upgraded:
	case <-time.After(2 * time.Second):
		t.Fatal("Engine exec stream was not attached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("exec cancellation error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Engine exec did not stop after context cancellation")
	}
}

func TestSDKDockerEngineWatchesMultiplexedContainerLogsAndExit(t *testing.T) {
	logsQuery := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/containers/devflow-pg-test/logs"):
			logsQuery <- request.URL.RawQuery
			response.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerLogFrame(t, response, stdcopy.Stdout, "postgres ready\npartial")
			writeDockerLogFrame(t, response, stdcopy.Stderr, "warning\r\n")
		case strings.HasSuffix(request.URL.Path, "/containers/devflow-pg-test/wait"):
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"StatusCode":0}`)
		default:
			http.Error(response, "unexpected Engine API request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := mobyclient.New(
		mobyclient.WithHost("tcp://"+server.Listener.Addr().String()),
		mobyclient.WithAPIVersion("1.55"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var lines []string
	engine := &sdkDockerEngine{client: client}
	if err := engine.WatchContainer(context.Background(), "devflow-pg-test", func(stream, line string) {
		lines = append(lines, stream+":"+line)
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, "|"); got != "stdout:postgres ready|stderr:warning|stdout:partial" {
		t.Fatalf("forwarded container logs = %q", got)
	}
	select {
	case query := <-logsQuery:
		for _, expected := range []string{"follow=1", "stderr=1", "stdout=1"} {
			if !strings.Contains(query, expected) {
				t.Fatalf("container logs query %q is missing %q", query, expected)
			}
		}
	default:
		t.Fatal("expected a container logs request")
	}
}

func TestSDKDockerEngineWatchCancellationClosesLogStream(t *testing.T) {
	logAttached := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/containers/devflow-pg-test/logs"):
			response.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerLogFrame(t, response, stdcopy.Stdout, "postgres starting\n")
			if flusher, ok := response.(http.Flusher); ok {
				flusher.Flush()
			}
			close(logAttached)
			<-request.Context().Done()
		case strings.HasSuffix(request.URL.Path, "/containers/devflow-pg-test/wait"):
			<-request.Context().Done()
		default:
			http.Error(response, "unexpected Engine API request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := mobyclient.New(
		mobyclient.WithHost("tcp://"+server.Listener.Addr().String()),
		mobyclient.WithAPIVersion("1.55"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	engine := &sdkDockerEngine{client: client}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- engine.WatchContainer(ctx, "devflow-pg-test", nil)
	}()
	select {
	case <-logAttached:
	case <-time.After(2 * time.Second):
		t.Fatal("container log stream was not attached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("watch cancellation error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("container watch did not stop after context cancellation")
	}
}

func writeDockerLogFrame(t *testing.T, writer io.Writer, stream stdcopy.StdType, payload string) {
	t.Helper()
	header := make([]byte, 8)
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	if _, err := writer.Write(append(header, payload...)); err != nil {
		t.Errorf("write Docker log frame: %v", err)
	}
}

func TestDockerfileBuildContextContainsOnlyEmbeddedRecipe(t *testing.T) {
	recipe := []byte("FROM postgres:18-bookworm\nRUN true\n")
	contextReader, err := dockerfileBuildContext(recipe)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(contextReader)
	header, err := archive.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "Dockerfile" || header.Mode != 0o600 {
		t.Fatalf("unexpected Dockerfile archive header: %+v", header)
	}
	contents, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(recipe) {
		t.Fatalf("Dockerfile contents = %q, want %q", contents, recipe)
	}
	if _, err := archive.Next(); err != io.EOF {
		t.Fatalf("expected one build-context entry, got %v", err)
	}
}

func writeDockerContext(t *testing.T, configDir, name, host string) {
	t.Helper()
	storeConfig := contextstore.NewConfig(
		nil,
		contextstore.EndpointTypeGetter(contextdocker.DockerEndpoint, func() any {
			return &contextdocker.EndpointMeta{}
		}),
	)
	store := contextstore.New(filepath.Join(configDir, "contexts"), storeConfig)
	if err := store.CreateOrUpdate(contextstore.Metadata{
		Name: name,
		Endpoints: map[string]any{
			contextdocker.DockerEndpoint: contextdocker.EndpointMeta{Host: host},
		},
	}); err != nil {
		t.Fatalf("create Docker context %s: %v", name, err)
	}
}

func clearDockerEndpointEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		mobyclient.EnvOverrideHost,
		mobyclient.EnvOverrideAPIVersion,
		mobyclient.EnvOverrideCertPath,
		mobyclient.EnvTLSVerify,
		"DOCKER_CONTEXT",
	} {
		t.Setenv(name, "")
	}
}

func TestParseDockerPlatform(t *testing.T) {
	platform, err := parseDockerPlatform("linux/arm64/v8")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%s/%s/%s", platform.OS, platform.Architecture, platform.Variant); got != "linux/arm64/v8" {
		t.Fatalf("parsed platform = %q", got)
	}
	if _, err := parseDockerPlatform("arm64"); err == nil {
		t.Fatal("expected incomplete Docker platform to fail")
	}
}

func TestDockerArchitectureMatchesHostAliases(t *testing.T) {
	for _, test := range []struct {
		host   string
		docker string
		want   bool
	}{
		{host: "amd64", docker: "amd64", want: true},
		{host: "amd64", docker: "x86_64", want: true},
		{host: "arm64", docker: "arm64", want: true},
		{host: "arm64", docker: "aarch64", want: true},
		{host: "arm64", docker: "amd64", want: false},
	} {
		if got := dockerArchitectureMatchesHost(test.host, test.docker); got != test.want {
			t.Fatalf("dockerArchitectureMatchesHost(%q, %q) = %v, want %v", test.host, test.docker, got, test.want)
		}
	}
}
