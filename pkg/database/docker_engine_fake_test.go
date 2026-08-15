package database

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// fakeDockerEngine keeps the broad migration/Prisma behavior suite focused on
// database orchestration while the SDK adapter has its own request-level tests.
// The legacy-looking call strings are test event labels only; no executable is
// started anywhere in this fake.
type fakeDockerEngine struct {
	runner        *fakeRunner
	imageInspects map[string]int
	execs         []dockerExecSpec
}

func newTestManager(runner *fakeRunner) *Manager {
	if runner == nil {
		runner = &fakeRunner{}
	}
	return newManagerWithDockerEngine(&fakeDockerEngine{
		runner:        runner,
		imageInspects: make(map[string]int),
	})
}

func (e *fakeDockerEngine) Ping(context.Context) error {
	return nil
}

func (e *fakeDockerEngine) Info(ctx context.Context) (dockerInfo, error) {
	out, err := e.runner.CombinedOutput(ctx, "docker", "info", "--format", "{{.Architecture}}")
	return dockerInfo{Architecture: strings.TrimSpace(string(out))}, err
}

func (e *fakeDockerEngine) InspectContainer(ctx context.Context, name string) (dockerContainer, bool, error) {
	out, err := e.runner.CombinedOutput(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		if fakeDockerMissing(err, "container", "object") {
			return dockerContainer{}, false, nil
		}
		return dockerContainer{}, false, err
	}
	container := dockerContainer{
		Running: strings.TrimSpace(string(out)) == "true",
		Ports:   make(map[int][]int),
	}
	portOut, err := e.runner.CombinedOutput(ctx, "docker", "inspect", "-f", fmt.Sprintf(`{{range (index .NetworkSettings.Ports "%d/tcp")}}{{.HostPort}}{{"\n"}}{{end}}`, DefaultContainerPort), name)
	if err != nil {
		return dockerContainer{}, false, err
	}
	for _, field := range strings.Fields(string(portOut)) {
		if port, err := strconv.Atoi(field); err == nil {
			container.Ports[DefaultContainerPort] = append(container.Ports[DefaultContainerPort], port)
		}
	}
	imageOut, err := e.runner.CombinedOutput(ctx, "docker", "inspect", "-f", "{{.Config.Image}}", name)
	if err != nil {
		return dockerContainer{}, false, err
	}
	container.Image = strings.TrimSpace(string(imageOut))
	mountOut, err := e.runner.CombinedOutput(ctx, "docker", "inspect", "-f", `{{range .Mounts}}{{println .Type .Name .Destination}}{{end}}`, name)
	if err != nil {
		return dockerContainer{}, false, err
	}
	fields := strings.Fields(string(mountOut))
	for index := 0; index+2 < len(fields); index += 3 {
		container.Mounts = append(container.Mounts, dockerMount{
			Type:        fields[index],
			Name:        fields[index+1],
			Destination: fields[index+2],
		})
	}
	return container, true, nil
}

func (e *fakeDockerEngine) RunContainer(ctx context.Context, spec dockerContainerSpec) error {
	args := []string{
		"run", "-d",
		"--name", spec.Name,
		"--label", "devflow.managed=true",
		"--label", "devflow.database=true",
		"-p", fmt.Sprintf("%d:%d", spec.HostPort, spec.ContainerPort),
	}
	for _, env := range spec.Env {
		args = append(args, "-e", env)
	}
	args = append(args, "-v", spec.VolumeName+":"+spec.VolumeDestination, spec.Image)
	_, err := e.runner.CombinedOutput(ctx, "docker", args...)
	return err
}

func (e *fakeDockerEngine) StartContainer(ctx context.Context, name string) error {
	_, err := e.runner.CombinedOutput(ctx, "docker", "start", name)
	return err
}

func (e *fakeDockerEngine) StopContainer(ctx context.Context, name string, timeoutSeconds int) error {
	_, err := e.runner.CombinedOutput(ctx, "docker", "stop", "-t", strconv.Itoa(timeoutSeconds), name)
	if fakeDockerMissing(err, "container", "object") {
		return nil
	}
	return err
}

func (e *fakeDockerEngine) RemoveContainer(ctx context.Context, name string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)
	_, err := e.runner.CombinedOutput(ctx, "docker", args...)
	if fakeDockerMissing(err, "container", "object") {
		return nil
	}
	return err
}

func (e *fakeDockerEngine) WatchContainer(ctx context.Context, name string, onLine func(string, string)) error {
	out, err := e.runner.CombinedOutput(ctx, "docker-engine", "watch-container", name)
	if onLine != nil {
		for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
			if line != "" {
				onLine("stdout", line)
			}
		}
	}
	return err
}

func (e *fakeDockerEngine) Exec(ctx context.Context, container string, spec dockerExecSpec) ([]byte, error) {
	spec.Command = append([]string(nil), spec.Command...)
	spec.Env = append([]string(nil), spec.Env...)
	spec.Stdin = append([]byte(nil), spec.Stdin...)
	e.execs = append(e.execs, spec)
	return e.runner.CombinedOutput(ctx, "docker", append([]string{"exec", container}, spec.Command...)...)
}

func (e *fakeDockerEngine) InspectVolume(ctx context.Context, name string) (bool, error) {
	_, err := e.runner.CombinedOutput(ctx, "docker", "volume", "inspect", name)
	if err != nil {
		if fakeDockerMissing(err, "volume", "object") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (e *fakeDockerEngine) CreateVolume(ctx context.Context, name string) error {
	_, err := e.runner.CombinedOutput(ctx, "docker", "volume", "create", name)
	return err
}

func (e *fakeDockerEngine) RemoveVolume(ctx context.Context, name string, force bool) error {
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)
	_, err := e.runner.CombinedOutput(ctx, "docker", args...)
	if fakeDockerMissing(err, "volume", "object") {
		return nil
	}
	return err
}

func (e *fakeDockerEngine) InspectImage(ctx context.Context, image string) (dockerImage, bool, error) {
	plainKey := key("docker", "image", "inspect", image)
	architectureKey := key("docker", "image", "inspect", "-f", "{{.Architecture}}", image)
	platformKey := platformInspectKey(image)
	_, hasPlain := e.runner.responses[plainKey]
	_, hasArchitecture := e.runner.responses[architectureKey]
	_, hasPlatform := e.runner.responses[platformKey]
	count := e.imageInspects[image]
	e.imageInspects[image] = count + 1

	var out []byte
	var err error
	info := dockerImage{}
	switch {
	case hasArchitecture && !hasPlatform:
		out, err = e.runner.CombinedOutput(ctx, "docker", "image", "inspect", "-f", "{{.Architecture}}", image)
		info.Architecture = strings.TrimSpace(string(out))
	case hasPlatform && count > 0:
		out, err = e.runner.CombinedOutput(ctx, "docker", "image", "inspect", "-f", "{{.Os}}/{{.Architecture}}", image)
		parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
		if len(parts) == 2 {
			info.OS = parts[0]
			info.Architecture = parts[1]
		}
	case hasPlain:
		_, err = e.runner.CombinedOutput(ctx, "docker", "image", "inspect", image)
	default:
		_, err = e.runner.CombinedOutput(ctx, "docker", "image", "inspect", image)
	}
	if err != nil {
		if fakeDockerMissing(err, "image", "object") {
			return dockerImage{}, false, nil
		}
		return dockerImage{}, false, err
	}
	return info, true, nil
}

func (e *fakeDockerEngine) PullImage(ctx context.Context, image string) error {
	_, err := e.runner.CombinedOutput(ctx, "docker", "pull", image)
	return err
}

func (e *fakeDockerEngine) BuildImage(ctx context.Context, spec dockerBuildSpec) error {
	e.runner.builds = append(e.runner.builds, spec)
	args := []string{
		"build", "--pull",
		"--platform", spec.Platform,
	}
	if postgresMajor, ok := spec.BuildArgs["POSTGRES_MAJOR"]; ok {
		args = append(args, "--build-arg", "POSTGRES_MAJOR="+postgresMajor)
	}
	args = append(args,
		"--label", "devflow.managed=true",
		"--label", "devflow.database=true",
		"-t", spec.Tag,
		"engine-api-context",
	)
	_, err := e.runner.CombinedOutput(ctx, "docker", args...)
	return err
}

func (e *fakeDockerEngine) ArchiveVolume(ctx context.Context, volumeName, sidecarImage, archivePath string) error {
	e.runner.calls = append(e.runner.calls, key("docker-engine", "archive-volume", volumeName, sidecarImage, archivePath))
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
		return err
	}
	archive, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(archive)
	if err := compressed.Close(); err != nil {
		_ = archive.Close()
		return err
	}
	return archive.Close()
}

func (e *fakeDockerEngine) RestoreVolume(_ context.Context, volumeName, sidecarImage, archivePath string) error {
	e.runner.calls = append(e.runner.calls, key("docker-engine", "restore-volume", volumeName, sidecarImage, archivePath))
	return nil
}

func fakeDockerMissing(err error, kinds ...string) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, kind := range kinds {
		if strings.Contains(message, "no such "+kind) {
			return true
		}
	}
	return false
}
