package database

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/api"
)

const (
	DefaultPostgresImage = "postgres:16.14"
	DefaultSidecarImage  = "alpine:3.24.1"
	DefaultContainerPort = 5432
	postgresDataMount    = "/var/lib/postgresql/data"
	postgres18DataMount  = "/var/lib/postgresql"
	postGISARMRecipe     = 1

	FlavorPostgres = "postgres"
	FlavorPostGIS  = "postgis"
)

//go:embed docker/postgis-arm64.Dockerfile
var postGISARMDockerfile []byte

var (
	dockerControlTimeout    = 15 * time.Second
	dockerDataTimeout       = 10 * time.Minute
	ErrSnapshotIncompatible = errors.New("database snapshot is incompatible with the current runtime")
)

type Config struct {
	Flavor          string
	PostgresVersion int
	Image           string
	SidecarImage    string
	Host            string
	HostPort        int
	ContainerPort   int
	User            string
	Password        string
	Database        string
	ContainerPrefix string
	VolumePrefix    string
	SnapshotRoot    string
}

type postGISRuntime struct {
	postgresVersion        int
	upstreamPostGISVersion string
	amd64Image             string
	arm64BaseImage         string
	arm64Image             string
}

type SnapshotManifest struct {
	Version         int       `json:"version"`
	Key             string    `json:"key"`
	CreatedAt       time.Time `json:"createdAt"`
	Image           string    `json:"image"`
	Platform        string    `json:"platform,omitempty"`
	PostgresVersion int       `json:"postgresVersion,omitempty"`
	SidecarImage    string    `json:"sidecarImage,omitempty"`
	ContainerName   string    `json:"containerName"`
	VolumeName      string    `json:"volumeName"`
	Database        string    `json:"database"`
	User            string    `json:"user"`
	Port            int       `json:"port"`
	ContainerPort   int       `json:"containerPort,omitempty"`
	ArchivePath     string    `json:"archivePath"`
}

type Manager struct {
	engine dockerEngine
}

func New() *Manager {
	return &Manager{}
}

func newManagerWithDockerEngine(engine dockerEngine) *Manager {
	return &Manager{engine: engine}
}

func (m *Manager) Desired(instanceID string, cfg Config) api.DBInstance {
	cfg = normalizeConfig(cfg)
	containerName := cfg.ContainerPrefix + instanceID
	volumeName := cfg.VolumePrefix + instanceID
	if cfg.Flavor == FlavorPostGIS && cfg.PostgresVersion > 0 {
		volumeName += "-pg" + strconv.Itoa(cfg.PostgresVersion)
	}
	return api.DBInstance{
		Name:            cfg.Database,
		URL:             postgresURL(cfg.Host, cfg.HostPort, cfg.User, cfg.Password, cfg.Database),
		Host:            cfg.Host,
		Port:            cfg.HostPort,
		ContainerPort:   cfg.ContainerPort,
		User:            cfg.User,
		Password:        cfg.Password,
		Flavor:          cfg.Flavor,
		PostgresVersion: cfg.PostgresVersion,
		Image:           cfg.Image,
		SidecarImage:    cfg.SidecarImage,
		ContainerName:   containerName,
		VolumeName:      volumeName,
		SnapshotRoot:    cfg.SnapshotRoot,
	}
}

func (m *Manager) EnsureRuntime(ctx context.Context, db api.DBInstance) error {
	if db.ContainerName == "" || db.VolumeName == "" {
		return fmt.Errorf("database container and volume names are required")
	}
	if db.Port == 0 {
		return fmt.Errorf("database host port is required")
	}
	image, err := m.resolveRuntimeImage(ctx, db)
	if err != nil {
		return err
	}
	containerPort := dbContainerPort(db)
	if db.User == "" || db.Password == "" || db.Name == "" {
		return fmt.Errorf("database name, user, and password are required")
	}

	container, exists, err := m.inspectContainer(ctx, db.ContainerName)
	if err != nil {
		return err
	}
	imageReady := false
	if exists {
		portOK := containerPublishesHostPort(container, db.Port, containerPort)
		imageOK := container.Image == image
		volumeOK := containerUsesVolume(container, db.VolumeName, postgresVolumeMount(db))
		if !portOK || !imageOK || !volumeOK {
			if err := m.ensureRuntimeImage(ctx, db, image); err != nil {
				return err
			}
			imageReady = true
			if err := m.removeContainer(ctx, db.ContainerName, true); err != nil {
				return err
			}
			container.Running = false
			exists = false
		}
	}
	if container.Running {
		return nil
	}
	if !exists && !imageReady {
		if err := m.ensureRuntimeImage(ctx, db, image); err != nil {
			return err
		}
	}
	if err := m.ensureVolume(ctx, db.VolumeName); err != nil {
		return err
	}
	if exists {
		return m.startContainer(ctx, db.ContainerName)
	}
	return m.runContainer(ctx, dockerContainerSpec{
		Name:  db.ContainerName,
		Image: image,
		Labels: map[string]string{
			"devflow.managed":  "true",
			"devflow.database": "true",
		},
		Env: []string{
			"POSTGRES_USER=" + db.User,
			"POSTGRES_PASSWORD=" + db.Password,
			"POSTGRES_DB=" + db.Name,
		},
		HostPort:          db.Port,
		ContainerPort:     containerPort,
		VolumeName:        db.VolumeName,
		VolumeDestination: postgresVolumeMount(db),
	})
}

func (m *Manager) WaitReady(ctx context.Context, db api.DBInstance, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		commandTimeout := minDuration(dockerControlTimeout, time.Until(deadline))
		readyCommand := []string{"pg_isready", "-U", db.User, "-d", db.Name}
		if db.ContainerPort > 0 && db.ContainerPort != DefaultContainerPort {
			readyCommand = append(readyCommand, "-p", strconv.Itoa(db.ContainerPort))
		}
		ready := false
		if _, err := m.execContainer(ctx, commandTimeout, db.ContainerName, readyCommand); err != nil {
			lastErr = err
		} else if strings.TrimSpace(db.Host) != "" {
			lastErr = hostPortReady(ctx, db.Host, db.Port, 200*time.Millisecond)
			if lastErr == nil {
				ready = true
			}
		} else {
			lastErr = nil
			ready = true
		}
		if ready {
			// The official image initializes through a temporary socket-only
			// server. Force TCP so only the final published runtime can pass.
			if _, err := m.execContainer(ctx, commandTimeout, db.ContainerName, psqlCommand(db, "SELECT 1", "127.0.0.1", true)); err != nil {
				lastErr = fmt.Errorf("connect to configured database %q: %w", db.Name, err)
				ready = false
			}
		}
		if ready {
			if db.Flavor == FlavorPostGIS {
				if _, err := m.execContainer(ctx, commandTimeout, db.ContainerName, psqlCommand(db, "CREATE EXTENSION IF NOT EXISTS postgis", "", false)); err != nil {
					lastErr = fmt.Errorf("enable PostGIS extension in database %q: %w", db.Name, err)
				} else {
					return nil
				}
			} else {
				return nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(minDuration(250*time.Millisecond, remaining)):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("database %q did not become ready within %s: %w", db.ContainerName, timeout, lastErr)
	}
	return fmt.Errorf("database %q did not become ready within %s", db.ContainerName, timeout)
}

func (m *Manager) WaitHostReady(ctx context.Context, db api.DBInstance, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := hostPortReady(ctx, db.Host, db.Port, 200*time.Millisecond); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("database host port %s did not become ready within %s", net.JoinHostPort(dbHost(db), strconv.Itoa(db.Port)), timeout)
}

func (m *Manager) StopRuntime(ctx context.Context, db api.DBInstance) error {
	if db.ContainerName == "" {
		return nil
	}
	return m.stopContainer(ctx, db.ContainerName, 10)
}

// StopRuntimeIfRunning reports whether a live container was actually stopped.
// Lifecycle result assembly uses this stricter form so stale database metadata
// cannot be presented to users as a successful stop.
func (m *Manager) StopRuntimeIfRunning(ctx context.Context, db api.DBInstance) (bool, error) {
	if db.ContainerName == "" {
		return false, nil
	}
	container, exists, err := m.inspectContainer(ctx, db.ContainerName)
	if err != nil {
		return false, err
	}
	if !exists || !container.Running {
		return false, nil
	}
	if err := m.stopContainer(ctx, db.ContainerName, 10); err != nil {
		return false, err
	}
	return true, nil
}

// ExecSQL executes a SQL statement inside the managed Postgres container
// through the Docker Engine API. Output is returned even when psql fails.
func (m *Manager) ExecSQL(ctx context.Context, db api.DBInstance, statement string) ([]byte, error) {
	if db.ContainerName == "" {
		return nil, fmt.Errorf("database container name is required")
	}
	if db.User == "" || db.Name == "" {
		return nil, fmt.Errorf("database user and name are required")
	}
	return m.execContainer(ctx, dockerDataTimeout, db.ContainerName, psqlCommand(db, statement, "", false))
}

func psqlCommand(db api.DBInstance, statement, host string, tuplesOnly bool) []string {
	command := []string{
		"psql", "-X", "-v", "ON_ERROR_STOP=1",
	}
	if host != "" {
		command = append(command, "-h", host)
	}
	command = append(command, "-U", db.User, "-d", db.Name)
	if db.ContainerPort > 0 && db.ContainerPort != DefaultContainerPort {
		command = append(command, "-p", strconv.Itoa(db.ContainerPort))
	}
	if tuplesOnly {
		command = append(command, "-At")
	}
	command = append(command, "-c", statement)
	return command
}

func (m *Manager) DestroyRuntime(ctx context.Context, db api.DBInstance, removeVolume bool) error {
	if db.ContainerName != "" {
		if err := m.removeContainer(ctx, db.ContainerName, true); err != nil {
			return err
		}
	}
	if removeVolume && db.VolumeName != "" {
		if err := m.removeVolume(ctx, db.VolumeName, true); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Snapshot(ctx context.Context, db api.DBInstance, key string) (*SnapshotManifest, error) {
	if db.SnapshotRoot == "" {
		return nil, fmt.Errorf("database snapshot root is required")
	}
	snapshotDir, err := databaseSnapshotDir(db.SnapshotRoot, key)
	if err != nil {
		return nil, err
	}
	image, platform, err := m.ensureRuntimeImagePlatform(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := m.ensureImage(ctx, dbSidecarImage(db)); err != nil {
		return nil, fmt.Errorf("prepare database snapshot sidecar image: %w", err)
	}
	if err := m.StopRuntime(ctx, db); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(snapshotDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return nil, err
	}
	archivePath := filepath.Join(snapshotDir, "volume.tgz")
	if err := m.archiveVolume(ctx, db.VolumeName, dbSidecarImage(db), archivePath); err != nil {
		return nil, err
	}
	manifest := &SnapshotManifest{
		Version:         3,
		Key:             key,
		CreatedAt:       time.Now().UTC(),
		Image:           image,
		Platform:        platform,
		PostgresVersion: db.PostgresVersion,
		SidecarImage:    dbSidecarImage(db),
		ContainerName:   db.ContainerName,
		VolumeName:      db.VolumeName,
		Database:        db.Name,
		User:            db.User,
		Port:            db.Port,
		ContainerPort:   dbContainerPort(db),
		ArchivePath:     archivePath,
	}
	if err := jsonutil.WriteFileAtomic(filepath.Join(snapshotDir, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func (m *Manager) RestoreSnapshot(ctx context.Context, db api.DBInstance, key string) (*SnapshotManifest, error) {
	if db.SnapshotRoot == "" {
		return nil, fmt.Errorf("database snapshot root is required")
	}
	snapshotDir, err := databaseSnapshotDir(db.SnapshotRoot, key)
	if err != nil {
		return nil, err
	}
	manifest, err := LoadSnapshot(filepath.Join(snapshotDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	image, platform, err := m.ensureRuntimeImagePlatform(ctx, db)
	if err != nil {
		return nil, err
	}
	if manifest.Image != "" && manifest.Image != image {
		return nil, fmt.Errorf("%w: snapshot %q uses image %s, current runtime uses %s", ErrSnapshotIncompatible, key, manifest.Image, image)
	}
	if db.PostgresVersion > 0 && manifest.PostgresVersion == 0 {
		return nil, fmt.Errorf("%w: snapshot %q has no recorded PostgreSQL version", ErrSnapshotIncompatible, key)
	}
	if db.PostgresVersion > 0 && manifest.PostgresVersion != db.PostgresVersion {
		return nil, fmt.Errorf("%w: snapshot %q uses PostgreSQL %d, current runtime uses PostgreSQL %d", ErrSnapshotIncompatible, key, manifest.PostgresVersion, db.PostgresVersion)
	}
	if platform != "" && manifest.Platform == "" {
		return nil, fmt.Errorf("%w: snapshot %q has no recorded image platform", ErrSnapshotIncompatible, key)
	}
	if platform != "" && manifest.Platform != platform {
		return nil, fmt.Errorf("%w: snapshot %q uses %s, current image uses %s", ErrSnapshotIncompatible, key, manifest.Platform, platform)
	}
	if err := m.ensureImage(ctx, dbSidecarImage(db)); err != nil {
		return nil, fmt.Errorf("prepare database restore sidecar image: %w", err)
	}
	if err := m.DestroyRuntime(ctx, db, true); err != nil {
		return nil, err
	}
	if err := m.ensureVolume(ctx, db.VolumeName); err != nil {
		return nil, err
	}
	if err := m.restoreVolume(ctx, db.VolumeName, dbSidecarImage(db), filepath.Join(snapshotDir, "volume.tgz")); err != nil {
		return nil, err
	}
	return manifest, nil
}

func LoadSnapshot(path string) (*SnapshotManifest, error) {
	var manifest SnapshotManifest
	if err := jsonutil.ReadFile(path, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func SnapshotKey(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, "_")
}

func databaseSnapshotDir(root, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("snapshot key is required")
	}
	if filepath.IsAbs(key) || filepath.VolumeName(key) != "" || filepath.Clean(key) != key || key == "." || key == ".." || strings.ContainsAny(key, `/\\`) {
		return "", fmt.Errorf("snapshot key %q must be a single directory name", key)
	}
	return filepath.Join(root, key), nil
}

func normalizeConfig(cfg Config) Config {
	if cfg.Flavor == "" {
		cfg.Flavor = FlavorPostgres
	}
	if cfg.Image == "" && cfg.Flavor == FlavorPostgres {
		cfg.Image = DefaultPostgresImage
	}
	if cfg.SidecarImage == "" {
		cfg.SidecarImage = DefaultSidecarImage
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.ContainerPort == 0 {
		cfg.ContainerPort = DefaultContainerPort
	}
	if cfg.User == "" {
		cfg.User = "devflow"
	}
	if cfg.Password == "" {
		cfg.Password = "devflow"
	}
	if cfg.Database == "" {
		cfg.Database = "devflow"
	}
	if cfg.ContainerPrefix == "" {
		cfg.ContainerPrefix = "devflow-pg-"
	}
	if cfg.VolumePrefix == "" {
		cfg.VolumePrefix = "devflow-pgdata-"
	}
	return cfg
}

func (m *Manager) inspectContainer(ctx context.Context, name string) (dockerContainer, bool, error) {
	type result struct {
		container dockerContainer
		exists    bool
	}
	inspected, err := dockerValue(m, ctx, dockerControlTimeout, "inspect Docker container "+name, func(ctx context.Context, engine dockerEngine) (result, error) {
		container, exists, err := engine.InspectContainer(ctx, name)
		return result{container: container, exists: exists}, err
	})
	return inspected.container, inspected.exists, err
}

func containerPublishesHostPort(container dockerContainer, hostPort, containerPort int) bool {
	for _, publishedPort := range container.Ports[containerPort] {
		if publishedPort == hostPort {
			return true
		}
	}
	return false
}

func containerUsesVolume(container dockerContainer, volume, destination string) bool {
	for _, mounted := range container.Mounts {
		if mounted.Type == "volume" && mounted.Name == volume && mounted.Destination == destination {
			return true
		}
	}
	return false
}

func (m *Manager) ensureVolume(ctx context.Context, name string) error {
	exists, err := dockerValue(m, ctx, dockerControlTimeout, "inspect Docker volume "+name, func(ctx context.Context, engine dockerEngine) (bool, error) {
		return engine.InspectVolume(ctx, name)
	})
	if err != nil || exists {
		return err
	}
	return m.dockerCall(ctx, dockerControlTimeout, "create Docker volume "+name, func(ctx context.Context, engine dockerEngine) error {
		return engine.CreateVolume(ctx, name)
	})
}

func (m *Manager) ensureImage(ctx context.Context, image string) error {
	_, exists, err := m.inspectImage(ctx, image)
	if err != nil || exists {
		return err
	}
	return m.dockerCall(ctx, dockerDataTimeout, "pull Docker image "+image, func(ctx context.Context, engine dockerEngine) error {
		return engine.PullImage(ctx, image)
	})
}

func (m *Manager) inspectImage(ctx context.Context, image string) (dockerImage, bool, error) {
	type result struct {
		image  dockerImage
		exists bool
	}
	inspected, err := dockerValue(m, ctx, dockerControlTimeout, "inspect Docker image "+image, func(ctx context.Context, engine dockerEngine) (result, error) {
		imageInfo, exists, err := engine.InspectImage(ctx, image)
		return result{image: imageInfo, exists: exists}, err
	})
	return inspected.image, inspected.exists, err
}

func (m *Manager) resolveRuntimeImage(ctx context.Context, db api.DBInstance) (string, error) {
	switch db.Flavor {
	case "", FlavorPostgres, FlavorPostGIS:
	default:
		return "", fmt.Errorf("unsupported database flavor %q", db.Flavor)
	}
	if db.Flavor == FlavorPostGIS {
		if _, err := postGISRuntimeForVersion(db.PostgresVersion); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(db.Image) != "" {
		return db.Image, nil
	}
	switch db.Flavor {
	case "", FlavorPostgres:
		return DefaultPostgresImage, nil
	case FlavorPostGIS:
		arch, err := m.dockerArchitecture(ctx)
		if err != nil {
			return "", fmt.Errorf("resolve Docker architecture for PostGIS: %w", err)
		}
		return postGISImageForArchitecture(db.PostgresVersion, arch)
	}
	return "", fmt.Errorf("unsupported database flavor %q", db.Flavor)
}

func postGISRuntimeForVersion(postgresVersion int) (postGISRuntime, error) {
	upstreamPostGISVersion := ""
	switch postgresVersion {
	case 16, 17:
		upstreamPostGISVersion = "3.5"
	case 18:
		upstreamPostGISVersion = "3.6"
	default:
		return postGISRuntime{}, fmt.Errorf("unsupported PostGIS PostgreSQL version %d; supported versions are 16, 17, and 18", postgresVersion)
	}
	return postGISRuntime{
		postgresVersion:        postgresVersion,
		upstreamPostGISVersion: upstreamPostGISVersion,
		amd64Image:             fmt.Sprintf("postgis/postgis:%d-%s", postgresVersion, upstreamPostGISVersion),
		arm64BaseImage:         fmt.Sprintf("postgres:%d-bookworm", postgresVersion),
		arm64Image:             fmt.Sprintf("devflow/postgis:%d-bookworm-postgis3-arm64-v%d", postgresVersion, postGISARMRecipe),
	}, nil
}

func postGISImageForArchitecture(postgresVersion int, arch string) (string, error) {
	runtime, err := postGISRuntimeForVersion(postgresVersion)
	if err != nil {
		return "", err
	}
	arch = strings.ToLower(strings.TrimSpace(arch))
	switch arch {
	case "amd64", "x86_64":
		return runtime.amd64Image, nil
	case "arm64", "aarch64":
		return runtime.arm64Image, nil
	default:
		return "", fmt.Errorf("PostGIS flavor does not support Docker architecture %q", arch)
	}
}

func (m *Manager) ensureRuntimeImage(ctx context.Context, db api.DBInstance, image string) error {
	if db.Flavor == FlavorPostGIS && strings.TrimSpace(db.Image) == "" {
		runtime, err := postGISRuntimeForVersion(db.PostgresVersion)
		if err != nil {
			return err
		}
		if image == runtime.arm64Image {
			return m.ensurePostGISARMImage(ctx, runtime)
		}
	}
	return m.ensureImage(ctx, image)
}

func (m *Manager) ensureRuntimeImagePlatform(ctx context.Context, db api.DBInstance) (string, string, error) {
	image, err := m.resolveRuntimeImage(ctx, db)
	if err != nil {
		return "", "", err
	}
	if err := m.ensureRuntimeImage(ctx, db, image); err != nil {
		return "", "", err
	}
	imageInfo, exists, err := m.inspectImage(ctx, image)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return "", "", fmt.Errorf("docker image %s disappeared after it was prepared", image)
	}
	platform := strings.Trim(strings.TrimSpace(imageInfo.OS)+"/"+strings.TrimSpace(imageInfo.Architecture), "/")
	return image, platform, nil
}

func (m *Manager) dockerArchitecture(ctx context.Context) (string, error) {
	info, err := dockerValue(m, ctx, dockerControlTimeout, "inspect Docker Engine", func(ctx context.Context, engine dockerEngine) (dockerInfo, error) {
		return engine.Info(ctx)
	})
	if err != nil {
		return "", err
	}
	arch := strings.ToLower(strings.TrimSpace(info.Architecture))
	if arch == "" {
		return "", fmt.Errorf("docker engine reported an empty architecture")
	}
	return arch, nil
}

func (m *Manager) ensurePostGISARMImage(ctx context.Context, runtime postGISRuntime) error {
	imageInfo, exists, err := m.inspectImage(ctx, runtime.arm64Image)
	if err == nil && exists {
		arch := strings.ToLower(strings.TrimSpace(imageInfo.Architecture))
		if arch == "arm64" || arch == "aarch64" {
			return nil
		}
	}
	if err != nil {
		return err
	}
	err = m.dockerCall(ctx, dockerDataTimeout, "build native arm64 PostGIS image "+runtime.arm64Image, func(ctx context.Context, engine dockerEngine) error {
		return engine.BuildImage(ctx, dockerBuildSpec{
			Tag:        runtime.arm64Image,
			Platform:   "linux/arm64",
			Dockerfile: append([]byte(nil), postGISARMDockerfile...),
			BuildArgs: map[string]string{
				"POSTGRES_MAJOR": strconv.Itoa(runtime.postgresVersion),
			},
			Labels: map[string]string{
				"devflow.managed":  "true",
				"devflow.database": "true",
			},
		})
	})
	if err != nil {
		return fmt.Errorf("build native arm64 PostGIS image from %s: %w", runtime.arm64BaseImage, err)
	}
	imageInfo, exists, err = m.inspectImage(ctx, runtime.arm64Image)
	if err != nil {
		return fmt.Errorf("inspect built arm64 PostGIS image: %w", err)
	}
	if !exists {
		return fmt.Errorf("built PostGIS image %s is missing after a successful build", runtime.arm64Image)
	}
	arch := strings.ToLower(strings.TrimSpace(imageInfo.Architecture))
	if arch != "arm64" && arch != "aarch64" {
		return fmt.Errorf("built PostGIS image %s has architecture %q, want arm64", runtime.arm64Image, arch)
	}
	return nil
}

func postgresVolumeMount(db api.DBInstance) string {
	if db.PostgresVersion >= 18 {
		return postgres18DataMount
	}
	return postgresDataMount
}

func postgresURL(host string, port int, user, password, database string) string {
	var userInfo *url.Userinfo
	if password != "" {
		userInfo = url.UserPassword(user, password)
	} else if user != "" {
		userInfo = url.User(user)
	}
	hostPort := host
	if port > 0 {
		hostPort = net.JoinHostPort(host, strconv.Itoa(port))
	}
	dsn := &url.URL{
		Scheme: "postgres",
		User:   userInfo,
		Host:   hostPort,
		Path:   "/" + database,
	}
	query := dsn.Query()
	query.Set("sslmode", "disable")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func dbContainerPort(db api.DBInstance) int {
	if db.ContainerPort > 0 {
		return db.ContainerPort
	}
	return DefaultContainerPort
}

func dbSidecarImage(db api.DBInstance) string {
	if strings.TrimSpace(db.SidecarImage) != "" {
		return db.SidecarImage
	}
	return DefaultSidecarImage
}

func hostPortReady(ctx context.Context, host string, port int, timeout time.Duration) error {
	if port == 0 {
		return fmt.Errorf("host port is required")
	}
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(dbHost(api.DBInstance{Host: host}), strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

func dbHost(db api.DBInstance) string {
	if strings.TrimSpace(db.Host) != "" {
		return db.Host
	}
	return "127.0.0.1"
}

func (m *Manager) dockerEngine() (dockerEngine, error) {
	if m != nil && m.engine != nil {
		return m.engine, nil
	}
	return defaultDockerEngine()
}

func (m *Manager) dockerCall(ctx context.Context, timeout time.Duration, operation string, call func(context.Context, dockerEngine) error) error {
	_, err := dockerValue(m, ctx, timeout, operation, func(ctx context.Context, engine dockerEngine) (struct{}, error) {
		return struct{}{}, call(ctx, engine)
	})
	return err
}

func dockerValue[T any](m *Manager, ctx context.Context, timeout time.Duration, operation string, call func(context.Context, dockerEngine) (T, error)) (T, error) {
	var zero T
	engine, err := m.dockerEngine()
	if err != nil {
		return zero, fmt.Errorf("initialize Docker Engine client: %w", err)
	}
	commandCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	value, err := call(commandCtx, engine)
	if err == nil {
		return value, nil
	}
	if timeout > 0 && errors.Is(commandCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return zero, fmt.Errorf("%s timed out after %s", operation, timeout)
	}
	return zero, fmt.Errorf("%s: %w", operation, err)
}

func (m *Manager) pingDocker(ctx context.Context) error {
	return m.dockerCall(ctx, dockerControlTimeout, "connect to Docker Engine", func(ctx context.Context, engine dockerEngine) error {
		return engine.Ping(ctx)
	})
}

func (m *Manager) runContainer(ctx context.Context, spec dockerContainerSpec) error {
	return m.dockerCall(ctx, dockerControlTimeout, "create and start Docker container "+spec.Name, func(ctx context.Context, engine dockerEngine) error {
		return engine.RunContainer(ctx, spec)
	})
}

func (m *Manager) startContainer(ctx context.Context, name string) error {
	return m.dockerCall(ctx, dockerControlTimeout, "start Docker container "+name, func(ctx context.Context, engine dockerEngine) error {
		return engine.StartContainer(ctx, name)
	})
}

func (m *Manager) stopContainer(ctx context.Context, name string, timeoutSeconds int) error {
	return m.dockerCall(ctx, dockerControlTimeout, "stop Docker container "+name, func(ctx context.Context, engine dockerEngine) error {
		return engine.StopContainer(ctx, name, timeoutSeconds)
	})
}

func (m *Manager) removeContainer(ctx context.Context, name string, force bool) error {
	return m.dockerCall(ctx, dockerControlTimeout, "remove Docker container "+name, func(ctx context.Context, engine dockerEngine) error {
		return engine.RemoveContainer(ctx, name, force)
	})
}

func (m *Manager) execContainer(ctx context.Context, timeout time.Duration, containerName string, command []string) ([]byte, error) {
	return m.execContainerSpec(ctx, timeout, containerName, dockerExecSpec{Command: command})
}

func (m *Manager) execContainerSpec(ctx context.Context, timeout time.Duration, containerName string, spec dockerExecSpec) ([]byte, error) {
	return dockerValue(m, ctx, timeout, "execute command in Docker container "+containerName, func(ctx context.Context, engine dockerEngine) ([]byte, error) {
		return engine.Exec(ctx, containerName, spec)
	})
}

func (m *Manager) watchContainer(ctx context.Context, containerName string, onLine func(string, string)) error {
	_, err := dockerValue(m, ctx, 0, "watch Docker container "+containerName, func(ctx context.Context, engine dockerEngine) (struct{}, error) {
		return struct{}{}, engine.WatchContainer(ctx, containerName, onLine)
	})
	return err
}

func (m *Manager) removeVolume(ctx context.Context, name string, force bool) error {
	return m.dockerCall(ctx, dockerControlTimeout, "remove Docker volume "+name, func(ctx context.Context, engine dockerEngine) error {
		return engine.RemoveVolume(ctx, name, force)
	})
}

func (m *Manager) archiveVolume(ctx context.Context, volumeName, sidecarImage, archivePath string) error {
	return m.dockerCall(ctx, dockerDataTimeout, "archive Docker volume "+volumeName, func(ctx context.Context, engine dockerEngine) error {
		return engine.ArchiveVolume(ctx, volumeName, sidecarImage, archivePath)
	})
}

func (m *Manager) restoreVolume(ctx context.Context, volumeName, sidecarImage, archivePath string) error {
	return m.dockerCall(ctx, dockerDataTimeout, "restore Docker volume "+volumeName, func(ctx context.Context, engine dockerEngine) error {
		return engine.RestoreVolume(ctx, volumeName, sidecarImage, archivePath)
	})
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}
