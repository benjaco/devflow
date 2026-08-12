package database

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	contextdocker "github.com/docker/cli/cli/context/docker"
	contextstore "github.com/docker/cli/cli/context/store"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/pkg/stdcopy"
	containertypes "github.com/moby/moby/api/types/container"
	mounttypes "github.com/moby/moby/api/types/mount"
	networktypes "github.com/moby/moby/api/types/network"
	registrytypes "github.com/moby/moby/api/types/registry"
	mobyclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	dockerDefaultContextName = "default"
	dockerHubAuthKey         = "https://index.docker.io/v1/"
	volumeSidecarMount       = "/devflow-volume"
)

type dockerInfo struct {
	Architecture string
}

type dockerContainer struct {
	Running bool
	Image   string
	Ports   map[int][]int
	Mounts  []dockerMount
}

type dockerMount struct {
	Type        string
	Name        string
	Destination string
}

type dockerImage struct {
	OS           string
	Architecture string
}

type dockerContainerSpec struct {
	Name              string
	Image             string
	Labels            map[string]string
	Env               []string
	HostPort          int
	ContainerPort     int
	VolumeName        string
	VolumeDestination string
}

type dockerBuildSpec struct {
	Tag        string
	Platform   string
	Dockerfile []byte
	BuildArgs  map[string]string
	Labels     map[string]string
}

// dockerEngine is deliberately smaller than mobyclient.APIClient. Keeping SDK
// types behind this boundary makes database behavior deterministic to unit
// test and prevents the rest of the package from depending on Docker internals.
type dockerEngine interface {
	Ping(context.Context) error
	Info(context.Context) (dockerInfo, error)
	InspectContainer(context.Context, string) (dockerContainer, bool, error)
	RunContainer(context.Context, dockerContainerSpec) error
	StartContainer(context.Context, string) error
	StopContainer(context.Context, string, int) error
	RemoveContainer(context.Context, string, bool) error
	WatchContainer(context.Context, string, func(string, string)) error
	Exec(context.Context, string, []string) ([]byte, error)
	InspectVolume(context.Context, string) (bool, error)
	CreateVolume(context.Context, string) error
	RemoveVolume(context.Context, string, bool) error
	InspectImage(context.Context, string) (dockerImage, bool, error)
	PullImage(context.Context, string) error
	BuildImage(context.Context, dockerBuildSpec) error
	ArchiveVolume(context.Context, string, string, string) error
	RestoreVolume(context.Context, string, string, string) error
}

type sdkDockerEngine struct {
	client *mobyclient.Client
	config *configfile.ConfigFile
}

var sharedDockerClient struct {
	sync.Mutex
	engine dockerEngine
}

func defaultDockerEngine() (dockerEngine, error) {
	sharedDockerClient.Lock()
	defer sharedDockerClient.Unlock()
	if sharedDockerClient.engine != nil {
		return sharedDockerClient.engine, nil
	}
	engine, err := newSDKDockerEngine()
	if err != nil {
		return nil, err
	}
	sharedDockerClient.engine = engine
	return engine, nil
}

func newSDKDockerEngine() (*sdkDockerEngine, error) {
	configDir, err := dockerConfigDir()
	if err != nil {
		return nil, err
	}
	client, configFile, err := newDockerAPIClientAt(configDir)
	if err != nil {
		return nil, err
	}
	return &sdkDockerEngine{client: client, config: configFile}, nil
}

func dockerConfigDir() (string, error) {
	if configured := strings.TrimSpace(os.Getenv(dockerconfig.EnvOverrideConfigDir)); configured != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for Docker configuration: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve home directory for Docker configuration: empty home directory")
	}
	return filepath.Join(home, ".docker"), nil
}

// newDockerAPIClientAt follows Docker CLI context precedence without invoking
// the docker executable: DOCKER_HOST, DOCKER_CONTEXT, currentContext, default.
// Endpoint transport setup is delegated to Docker's own context package, which
// supports Unix sockets, Windows named pipes, TCP/TLS, and SSH contexts.
func newDockerAPIClientAt(configDir string) (*mobyclient.Client, *configfile.ConfigFile, error) {
	configFile, err := dockerconfig.Load(configDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load Docker configuration from %s: %w", configDir, err)
	}

	if strings.TrimSpace(os.Getenv(mobyclient.EnvOverrideHost)) != "" {
		client, err := mobyclient.New(mobyclient.FromEnv)
		if err != nil {
			return nil, nil, fmt.Errorf("configure Docker Engine client from environment: %w", err)
		}
		return client, configFile, nil
	}

	contextName := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT"))
	if contextName == "" {
		contextName = strings.TrimSpace(configFile.CurrentContext)
	}
	if contextName == "" || contextName == dockerDefaultContextName {
		client, err := mobyclient.New(mobyclient.FromEnv)
		if err != nil {
			return nil, nil, fmt.Errorf("configure default Docker Engine client: %w", err)
		}
		return client, configFile, nil
	}

	storeConfig := contextstore.NewConfig(
		nil,
		contextstore.EndpointTypeGetter(contextdocker.DockerEndpoint, func() any {
			return &contextdocker.EndpointMeta{}
		}),
	)
	contextStore := contextstore.New(filepath.Join(configDir, "contexts"), storeConfig)
	metadata, err := contextStore.GetMetadata(contextName)
	if err != nil {
		return nil, nil, fmt.Errorf("load Docker context %q: %w", contextName, err)
	}
	endpointMetadata, err := contextdocker.EndpointFromContext(metadata)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve Docker endpoint for context %q: %w", contextName, err)
	}
	endpoint, err := contextdocker.WithTLSData(contextStore, contextName, endpointMetadata)
	if err != nil {
		return nil, nil, fmt.Errorf("load Docker TLS data for context %q: %w", contextName, err)
	}
	options, err := endpoint.ClientOpts()
	if err != nil {
		return nil, nil, fmt.Errorf("configure Docker endpoint for context %q: %w", contextName, err)
	}
	client, err := mobyclient.New(options...)
	if err != nil {
		return nil, nil, fmt.Errorf("create Docker Engine client for context %q: %w", contextName, err)
	}
	return client, configFile, nil
}

func (e *sdkDockerEngine) Ping(ctx context.Context) error {
	_, err := e.client.Ping(ctx, mobyclient.PingOptions{})
	return err
}

func (e *sdkDockerEngine) Info(ctx context.Context) (dockerInfo, error) {
	result, err := e.client.Info(ctx, mobyclient.InfoOptions{})
	if err != nil {
		return dockerInfo{}, err
	}
	return dockerInfo{Architecture: result.Info.Architecture}, nil
}

func (e *sdkDockerEngine) InspectContainer(ctx context.Context, name string) (dockerContainer, bool, error) {
	result, err := e.client.ContainerInspect(ctx, name, mobyclient.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return dockerContainer{}, false, nil
		}
		return dockerContainer{}, false, err
	}
	inspected := result.Container
	container := dockerContainer{
		Ports: make(map[int][]int),
	}
	if inspected.State != nil {
		container.Running = inspected.State.Running
	}
	if inspected.Config != nil {
		container.Image = inspected.Config.Image
	}
	if inspected.NetworkSettings != nil {
		for port, bindings := range inspected.NetworkSettings.Ports {
			containerPort, err := strconv.Atoi(port.Port())
			if err != nil {
				continue
			}
			for _, binding := range bindings {
				hostPort, err := strconv.Atoi(binding.HostPort)
				if err == nil {
					container.Ports[containerPort] = append(container.Ports[containerPort], hostPort)
				}
			}
		}
	}
	for _, mounted := range inspected.Mounts {
		container.Mounts = append(container.Mounts, dockerMount{
			Type:        string(mounted.Type),
			Name:        mounted.Name,
			Destination: mounted.Destination,
		})
	}
	return container, true, nil
}

func (e *sdkDockerEngine) RunContainer(ctx context.Context, spec dockerContainerSpec) error {
	port, err := networktypes.ParsePort(strconv.Itoa(spec.ContainerPort) + "/tcp")
	if err != nil {
		return fmt.Errorf("parse container port %d: %w", spec.ContainerPort, err)
	}
	result, err := e.client.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Name: spec.Name,
		Config: &containertypes.Config{
			Image:        spec.Image,
			Env:          append([]string(nil), spec.Env...),
			Labels:       cloneStringMap(spec.Labels),
			ExposedPorts: networktypes.PortSet{port: {}},
		},
		HostConfig: &containertypes.HostConfig{
			PortBindings: networktypes.PortMap{
				port: {{HostPort: strconv.Itoa(spec.HostPort)}},
			},
			Mounts: []mounttypes.Mount{{
				Type:   mounttypes.TypeVolume,
				Source: spec.VolumeName,
				Target: spec.VolumeDestination,
			}},
		},
	})
	if err != nil {
		return err
	}
	if _, err := e.client.ContainerStart(ctx, result.ID, mobyclient.ContainerStartOptions{}); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerControlTimeout)
		defer cancel()
		_, cleanupErr := e.client.ContainerRemove(cleanupCtx, result.ID, mobyclient.ContainerRemoveOptions{Force: true})
		if cleanupErr != nil && !cerrdefs.IsNotFound(cleanupErr) {
			return fmt.Errorf("start container: %w (cleanup failed: %v)", err, cleanupErr)
		}
		return err
	}
	return nil
}

func (e *sdkDockerEngine) StartContainer(ctx context.Context, name string) error {
	_, err := e.client.ContainerStart(ctx, name, mobyclient.ContainerStartOptions{})
	return err
}

func (e *sdkDockerEngine) StopContainer(ctx context.Context, name string, timeoutSeconds int) error {
	_, err := e.client.ContainerStop(ctx, name, mobyclient.ContainerStopOptions{Timeout: &timeoutSeconds})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *sdkDockerEngine) RemoveContainer(ctx context.Context, name string, force bool) error {
	_, err := e.client.ContainerRemove(ctx, name, mobyclient.ContainerRemoveOptions{Force: force})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *sdkDockerEngine) WatchContainer(ctx context.Context, name string, onLine func(string, string)) error {
	logs, err := e.client.ContainerLogs(ctx, name, mobyclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "all",
	})
	if err != nil {
		return err
	}
	defer logs.Close()

	stdout := newDockerLogLineWriter("stdout", onLine)
	stderr := newDockerLogLineWriter("stderr", onLine)
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(stdout, stderr, logs)
		stdout.Flush()
		stderr.Flush()
		copyDone <- copyErr
	}()

	wait := e.client.ContainerWait(ctx, name, mobyclient.ContainerWaitOptions{
		Condition: containertypes.WaitConditionNotRunning,
	})
	copyResult := (<-chan error)(copyDone)
	finishLogCopy := func(closeStream bool) error {
		if copyResult == nil {
			return nil
		}
		if closeStream {
			_ = logs.Close()
		}
		select {
		case copyErr := <-copyResult:
			copyResult = nil
			return copyErr
		case <-ctx.Done():
			_ = logs.Close()
			<-copyResult
			copyResult = nil
			return ctx.Err()
		}
	}
	for {
		select {
		case copyErr := <-copyResult:
			copyResult = nil
			if copyErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("follow container logs: %w", copyErr)
			}
		case result := <-wait.Result:
			// A successful wait means the container has stopped, but the followed
			// HTTP log stream can still have buffered frames to deliver. Let it
			// reach EOF naturally; closing it here races the copy and produces
			// http.ErrBodyReadAfterClose under the race detector.
			if copyErr := finishLogCopy(false); copyErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("finish container logs: %w", copyErr)
			}
			if result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
				return fmt.Errorf("wait for container: %s", result.Error.Message)
			}
			if result.StatusCode != 0 {
				return &dockerContainerExitError{name: name, code: result.StatusCode}
			}
			return nil
		case waitErr := <-wait.Error:
			_ = finishLogCopy(true)
			if waitErr == nil && ctx.Err() != nil {
				return ctx.Err()
			}
			return waitErr
		case <-ctx.Done():
			_ = finishLogCopy(true)
			return ctx.Err()
		}
	}
}

type dockerContainerExitError struct {
	name string
	code int64
}

func (e *dockerContainerExitError) Error() string {
	return fmt.Sprintf("container %s exited with code %d", e.name, e.code)
}

type dockerLogLineWriter struct {
	stream string
	onLine func(string, string)
	buffer []byte
}

func newDockerLogLineWriter(stream string, onLine func(string, string)) *dockerLogLineWriter {
	return &dockerLogLineWriter{stream: stream, onLine: onLine}
}

func (w *dockerLogLineWriter) Write(data []byte) (int, error) {
	w.buffer = append(w.buffer, data...)
	for {
		newline := bytes.IndexByte(w.buffer, '\n')
		if newline < 0 {
			break
		}
		w.emit(w.buffer[:newline])
		w.buffer = w.buffer[newline+1:]
	}
	return len(data), nil
}

func (w *dockerLogLineWriter) Flush() {
	if len(w.buffer) == 0 {
		return
	}
	w.emit(w.buffer)
	w.buffer = nil
}

func (w *dockerLogLineWriter) emit(line []byte) {
	if w.onLine != nil {
		w.onLine(w.stream, strings.TrimSuffix(string(line), "\r"))
	}
}

func (e *sdkDockerEngine) Exec(ctx context.Context, containerName string, command []string) ([]byte, error) {
	created, err := e.client.ExecCreate(ctx, containerName, mobyclient.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          append([]string(nil), command...),
	})
	if err != nil {
		return nil, err
	}
	attached, err := e.client.ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	defer attached.Close()

	copyDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			attached.Close()
		case <-copyDone:
		}
	}()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, copyErr := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
	close(copyDone)
	if ctx.Err() != nil {
		return combinedDockerOutput(stdout.Bytes(), stderr.Bytes()), ctx.Err()
	}
	if copyErr != nil {
		return combinedDockerOutput(stdout.Bytes(), stderr.Bytes()), copyErr
	}
	inspected, err := e.client.ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
	if err != nil {
		return combinedDockerOutput(stdout.Bytes(), stderr.Bytes()), err
	}
	output := combinedDockerOutput(stdout.Bytes(), stderr.Bytes())
	if inspected.ExitCode != 0 {
		return output, &dockerExecError{
			container: containerName,
			command:   append([]string(nil), command...),
			exitCode:  inspected.ExitCode,
			output:    output,
		}
	}
	return output, nil
}

type dockerExecError struct {
	container string
	command   []string
	exitCode  int
	output    []byte
}

func (e *dockerExecError) Error() string {
	message := fmt.Sprintf("Docker exec in %s exited with code %d", e.container, e.exitCode)
	if command := strings.Join(e.command, " "); command != "" {
		message += ": " + command
	}
	if output := strings.TrimSpace(string(e.output)); output != "" {
		message += ": " + output
	}
	return message
}

func combinedDockerOutput(stdout, stderr []byte) []byte {
	combined := make([]byte, 0, len(stdout)+len(stderr)+1)
	combined = append(combined, stdout...)
	if len(stderr) > 0 {
		if len(combined) > 0 && combined[len(combined)-1] != '\n' {
			combined = append(combined, '\n')
		}
		combined = append(combined, stderr...)
	}
	return combined
}

func (e *sdkDockerEngine) InspectVolume(ctx context.Context, name string) (bool, error) {
	_, err := e.client.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (e *sdkDockerEngine) CreateVolume(ctx context.Context, name string) error {
	_, err := e.client.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{
		Name: name,
		Labels: map[string]string{
			"devflow.managed":  "true",
			"devflow.database": "true",
		},
	})
	return err
}

func (e *sdkDockerEngine) RemoveVolume(ctx context.Context, name string, force bool) error {
	_, err := e.client.VolumeRemove(ctx, name, mobyclient.VolumeRemoveOptions{Force: force})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *sdkDockerEngine) InspectImage(ctx context.Context, image string) (dockerImage, bool, error) {
	result, err := e.client.ImageInspect(ctx, image)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return dockerImage{}, false, nil
		}
		return dockerImage{}, false, err
	}
	return dockerImage{OS: result.Os, Architecture: result.Architecture}, true, nil
}

func (e *sdkDockerEngine) PullImage(ctx context.Context, image string) error {
	registryAuth, err := e.registryAuth(image)
	if err != nil {
		return err
	}
	response, err := e.client.ImagePull(ctx, image, mobyclient.ImagePullOptions{RegistryAuth: registryAuth})
	if err != nil {
		return err
	}
	return response.Wait(ctx)
}

func (e *sdkDockerEngine) registryAuth(image string) (string, error) {
	if e.config == nil {
		return "", nil
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("parse image reference %q: %w", image, err)
	}
	registryHost := reference.Domain(named)
	authKey := registryHost
	if registryHost == "docker.io" {
		authKey = dockerHubAuthKey
	}
	configured, err := e.config.GetAuthConfig(authKey)
	if err != nil {
		return "", fmt.Errorf("load registry credentials for %s: %w", registryHost, err)
	}
	if configured.Username == "" && configured.Password == "" && configured.Auth == "" && configured.IdentityToken == "" && configured.RegistryToken == "" {
		return "", nil
	}
	return authconfig.Encode(registrytypes.AuthConfig{
		Username:      configured.Username,
		Password:      configured.Password,
		Auth:          configured.Auth,
		IdentityToken: configured.IdentityToken,
		RegistryToken: configured.RegistryToken,
		ServerAddress: configured.ServerAddress,
	})
}

func (e *sdkDockerEngine) BuildImage(ctx context.Context, spec dockerBuildSpec) error {
	buildContext, err := dockerfileBuildContext(spec.Dockerfile)
	if err != nil {
		return err
	}
	platform, err := parseDockerPlatform(spec.Platform)
	if err != nil {
		return err
	}
	buildArgs := make(map[string]*string, len(spec.BuildArgs))
	for name, value := range spec.BuildArgs {
		valueCopy := value
		buildArgs[name] = &valueCopy
	}
	result, err := e.client.ImageBuild(ctx, buildContext, mobyclient.ImageBuildOptions{
		Tags:       []string{spec.Tag},
		Dockerfile: "Dockerfile",
		PullParent: true,
		Remove:     true,
		BuildArgs:  buildArgs,
		Labels:     cloneStringMap(spec.Labels),
		Platforms:  []ocispec.Platform{platform},
	})
	if err != nil {
		return err
	}
	defer result.Body.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(result.Body, io.Discard, 0, false, nil); err != nil {
		return err
	}
	return nil
}

func dockerfileBuildContext(dockerfile []byte) (io.Reader, error) {
	var contextArchive bytes.Buffer
	writer := tar.NewWriter(&contextArchive)
	header := &tar.Header{
		Name:     "Dockerfile",
		Mode:     0o600,
		Size:     int64(len(dockerfile)),
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
	}
	if err := writer.WriteHeader(header); err != nil {
		return nil, fmt.Errorf("write PostGIS build-context header: %w", err)
	}
	if _, err := writer.Write(dockerfile); err != nil {
		return nil, fmt.Errorf("write PostGIS Dockerfile to build context: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close PostGIS build context: %w", err)
	}
	return bytes.NewReader(contextArchive.Bytes()), nil
}

func parseDockerPlatform(platform string) (ocispec.Platform, error) {
	parts := strings.Split(strings.TrimSpace(platform), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return ocispec.Platform{}, fmt.Errorf("invalid Docker platform %q", platform)
	}
	parsed := ocispec.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		parsed.Variant = parts[2]
	}
	return parsed, nil
}

func (e *sdkDockerEngine) ArchiveVolume(ctx context.Context, volumeName, sidecarImage, archivePath string) error {
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
		return fmt.Errorf("create database snapshot directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(archivePath), ".volume-*.tgz")
	if err != nil {
		return fmt.Errorf("create temporary database snapshot archive: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary database snapshot archive: %w", err)
	}
	compressed := gzip.NewWriter(temporary)
	err = e.withVolumeSidecar(ctx, volumeName, sidecarImage, func(containerID string) error {
		result, err := e.client.CopyFromContainer(ctx, containerID, mobyclient.CopyFromContainerOptions{
			SourcePath: volumeSidecarMount + "/.",
		})
		if err != nil {
			return err
		}
		defer result.Content.Close()
		_, err = io.Copy(compressed, result.Content)
		return err
	})
	closeErr := compressed.Close()
	if err != nil {
		return fmt.Errorf("archive Docker volume %s: %w", volumeName, err)
	}
	if closeErr != nil {
		return fmt.Errorf("finish database snapshot compression: %w", closeErr)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync database snapshot archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close database snapshot archive: %w", err)
	}
	if err := os.Rename(temporaryPath, archivePath); err != nil {
		return fmt.Errorf("commit database snapshot archive: %w", err)
	}
	committed = true
	return nil
}

func (e *sdkDockerEngine) RestoreVolume(ctx context.Context, volumeName, sidecarImage, archivePath string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open database snapshot archive: %w", err)
	}
	defer archive.Close()
	decompressed, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("read database snapshot archive: %w", err)
	}
	defer decompressed.Close()
	return e.withVolumeSidecar(ctx, volumeName, sidecarImage, func(containerID string) error {
		_, err := e.client.CopyToContainer(ctx, containerID, mobyclient.CopyToContainerOptions{
			DestinationPath: volumeSidecarMount,
			Content:         decompressed,
			CopyUIDGID:      true,
		})
		return err
	})
}

func (e *sdkDockerEngine) withVolumeSidecar(ctx context.Context, volumeName, sidecarImage string, work func(string) error) error {
	created, err := e.client.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &containertypes.Config{
			Image: sidecarImage,
			Cmd:   []string{"sh", "-c", "while :; do sleep 3600; done"},
			Labels: map[string]string{
				"devflow.managed":  "true",
				"devflow.database": "true",
				"devflow.sidecar":  "true",
			},
		},
		HostConfig: &containertypes.HostConfig{
			Mounts: []mounttypes.Mount{{
				Type:   mounttypes.TypeVolume,
				Source: volumeName,
				Target: volumeSidecarMount,
			}},
		},
	})
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerControlTimeout)
		defer cancel()
		_, _ = e.client.ContainerRemove(cleanupCtx, created.ID, mobyclient.ContainerRemoveOptions{Force: true})
	}()
	if _, err := e.client.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err != nil {
		return err
	}
	return work(created.ID)
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}
