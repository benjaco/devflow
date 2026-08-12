package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
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
)

var (
	dockerControlTimeout    = 15 * time.Second
	dockerDataTimeout       = 10 * time.Minute
	ErrSnapshotIncompatible = errors.New("database snapshot is incompatible with the current runtime")
)

type Config struct {
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

type SnapshotManifest struct {
	Version       int       `json:"version"`
	Key           string    `json:"key"`
	CreatedAt     time.Time `json:"createdAt"`
	Image         string    `json:"image"`
	Platform      string    `json:"platform,omitempty"`
	SidecarImage  string    `json:"sidecarImage,omitempty"`
	ContainerName string    `json:"containerName"`
	VolumeName    string    `json:"volumeName"`
	Database      string    `json:"database"`
	User          string    `json:"user"`
	Port          int       `json:"port"`
	ContainerPort int       `json:"containerPort,omitempty"`
	ArchivePath   string    `json:"archivePath"`
}

type Runner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
}

type commandOutputError struct {
	err    error
	output []byte
}

func (e *commandOutputError) Error() string {
	text := strings.TrimSpace(string(e.output))
	if text == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + text
}

func (e *commandOutputError) Unwrap() error {
	return e.err
}

type Manager struct {
	runner Runner
}

func New() *Manager {
	return &Manager{runner: execRunner{}}
}

func NewWithRunner(runner Runner) *Manager {
	if runner == nil {
		runner = execRunner{}
	}
	return &Manager{runner: runner}
}

func (m *Manager) Desired(instanceID string, cfg Config) api.DBInstance {
	cfg = normalizeConfig(cfg)
	containerName := cfg.ContainerPrefix + instanceID
	volumeName := cfg.VolumePrefix + instanceID
	return api.DBInstance{
		Name:          cfg.Database,
		URL:           postgresURL(cfg.Host, cfg.HostPort, cfg.User, cfg.Password, cfg.Database),
		Host:          cfg.Host,
		Port:          cfg.HostPort,
		ContainerPort: cfg.ContainerPort,
		User:          cfg.User,
		Password:      cfg.Password,
		Image:         cfg.Image,
		SidecarImage:  cfg.SidecarImage,
		ContainerName: containerName,
		VolumeName:    volumeName,
		SnapshotRoot:  cfg.SnapshotRoot,
	}
}

func (m *Manager) EnsureRuntime(ctx context.Context, db api.DBInstance) error {
	if db.ContainerName == "" || db.VolumeName == "" {
		return fmt.Errorf("database container and volume names are required")
	}
	if db.Port == 0 {
		return fmt.Errorf("database host port is required")
	}
	if db.Image == "" {
		db.Image = DefaultPostgresImage
	}
	containerPort := dbContainerPort(db)
	if db.User == "" || db.Password == "" || db.Name == "" {
		return fmt.Errorf("database name, user, and password are required")
	}

	running, exists, err := m.inspectContainer(ctx, db.ContainerName)
	if err != nil {
		return err
	}
	imageReady := false
	if exists {
		portOK, err := m.containerPublishesHostPort(ctx, db.ContainerName, db.Port, containerPort)
		if err != nil {
			return err
		}
		imageOK, err := m.containerUsesImage(ctx, db.ContainerName, db.Image)
		if err != nil {
			return err
		}
		if !portOK || !imageOK {
			if err := m.ensureImage(ctx, db.Image); err != nil {
				return err
			}
			imageReady = true
			if _, err := m.runDocker(ctx, dockerControlTimeout, "rm", "-f", db.ContainerName); err != nil && !containerMissing(err) {
				return err
			}
			running = false
			exists = false
		}
	}
	if running {
		return nil
	}
	if !exists && !imageReady {
		if err := m.ensureImage(ctx, db.Image); err != nil {
			return err
		}
	}
	if err := m.ensureVolume(ctx, db.VolumeName); err != nil {
		return err
	}
	if exists {
		_, err := m.runDocker(ctx, dockerControlTimeout, "start", db.ContainerName)
		return err
	}
	_, err = m.runDocker(ctx, dockerControlTimeout, "run", "-d",
		"--name", db.ContainerName,
		"--label", "devflow.managed=true",
		"--label", "devflow.database=true",
		"-p", fmt.Sprintf("%d:%d", db.Port, containerPort),
		"-e", "POSTGRES_USER="+db.User,
		"-e", "POSTGRES_PASSWORD="+db.Password,
		"-e", "POSTGRES_DB="+db.Name,
		"-v", db.VolumeName+":/var/lib/postgresql/data",
		db.Image,
	)
	return err
}

func (m *Manager) WaitReady(ctx context.Context, db api.DBInstance, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		commandTimeout := minDuration(dockerControlTimeout, time.Until(deadline))
		readyArgs := []string{"exec", db.ContainerName, "pg_isready", "-U", db.User, "-d", db.Name}
		if db.ContainerPort > 0 && db.ContainerPort != DefaultContainerPort {
			readyArgs = append(readyArgs, "-p", strconv.Itoa(db.ContainerPort))
		}
		_, err := m.runDocker(ctx, commandTimeout, readyArgs...)
		if err == nil && (strings.TrimSpace(db.Host) == "" || hostPortReady(ctx, db.Host, db.Port, 200*time.Millisecond) == nil) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
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
	_, err := m.runDocker(ctx, dockerControlTimeout, "stop", "-t", "10", db.ContainerName)
	if containerMissing(err) {
		return nil
	}
	return err
}

func (m *Manager) DestroyRuntime(ctx context.Context, db api.DBInstance, removeVolume bool) error {
	if db.ContainerName != "" {
		_, err := m.runDocker(ctx, dockerControlTimeout, "rm", "-f", db.ContainerName)
		if err != nil && !containerMissing(err) {
			return err
		}
	}
	if removeVolume && db.VolumeName != "" {
		_, err := m.runDocker(ctx, dockerControlTimeout, "volume", "rm", "-f", db.VolumeName)
		if err != nil && !volumeMissing(err) {
			return err
		}
	}
	return nil
}

func (m *Manager) Snapshot(ctx context.Context, db api.DBInstance, key string) (*SnapshotManifest, error) {
	if db.SnapshotRoot == "" {
		return nil, fmt.Errorf("database snapshot root is required")
	}
	if key == "" {
		return nil, fmt.Errorf("snapshot key is required")
	}
	image := db.Image
	if image == "" {
		image = DefaultPostgresImage
	}
	platform, err := m.ensureImagePlatform(ctx, image)
	if err != nil {
		return nil, err
	}
	if err := m.StopRuntime(ctx, db); err != nil {
		return nil, err
	}
	snapshotDir := filepath.Join(db.SnapshotRoot, key)
	if err := os.RemoveAll(snapshotDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		return nil, err
	}
	archivePath := filepath.Join(snapshotDir, "volume.tgz")
	_, err = m.runDocker(ctx, dockerDataTimeout, "run", "--rm",
		"-v", db.VolumeName+":/from",
		"-v", snapshotDir+":/to",
		dbSidecarImage(db),
		"sh", "-c", "cd /from && tar czf /to/volume.tgz .",
	)
	if err != nil {
		return nil, err
	}
	manifest := &SnapshotManifest{
		Version:       2,
		Key:           key,
		CreatedAt:     time.Now().UTC(),
		Image:         image,
		Platform:      platform,
		SidecarImage:  dbSidecarImage(db),
		ContainerName: db.ContainerName,
		VolumeName:    db.VolumeName,
		Database:      db.Name,
		User:          db.User,
		Port:          db.Port,
		ContainerPort: dbContainerPort(db),
		ArchivePath:   archivePath,
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
	if key == "" {
		return nil, fmt.Errorf("snapshot key is required")
	}
	snapshotDir := filepath.Join(db.SnapshotRoot, key)
	manifest, err := LoadSnapshot(filepath.Join(snapshotDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	image := db.Image
	if image == "" {
		image = DefaultPostgresImage
	}
	platform, err := m.ensureImagePlatform(ctx, image)
	if err != nil {
		return nil, err
	}
	if platform != "" && manifest.Platform == "" {
		return nil, fmt.Errorf("%w: snapshot %q has no recorded image platform", ErrSnapshotIncompatible, key)
	}
	if platform != "" && manifest.Platform != platform {
		return nil, fmt.Errorf("%w: snapshot %q uses %s, current image uses %s", ErrSnapshotIncompatible, key, manifest.Platform, platform)
	}
	if err := m.DestroyRuntime(ctx, db, true); err != nil {
		return nil, err
	}
	if err := m.ensureVolume(ctx, db.VolumeName); err != nil {
		return nil, err
	}
	_, err = m.runDocker(ctx, dockerDataTimeout, "run", "--rm",
		"-v", db.VolumeName+":/to",
		"-v", snapshotDir+":/from",
		dbSidecarImage(db),
		"sh", "-c", "cd /to && tar xzf /from/volume.tgz",
	)
	if err != nil {
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

func normalizeConfig(cfg Config) Config {
	if cfg.Image == "" {
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

func (m *Manager) inspectContainer(ctx context.Context, name string) (running bool, exists bool, err error) {
	out, inspectErr := m.runDocker(ctx, dockerControlTimeout, "inspect", "-f", "{{.State.Running}}", name)
	if inspectErr != nil {
		if containerMissing(inspectErr) {
			return false, false, nil
		}
		return false, false, inspectErr
	}
	return strings.TrimSpace(string(out)) == "true", true, nil
}

func (m *Manager) containerPublishesHostPort(ctx context.Context, name string, hostPort, containerPort int) (bool, error) {
	format := fmt.Sprintf(`{{range (index .NetworkSettings.Ports "%d/tcp")}}{{.HostPort}}{{"\n"}}{{end}}`, containerPort)
	out, err := m.runDocker(ctx, dockerControlTimeout, "inspect", "-f", format, name)
	if containerMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	want := strconv.Itoa(hostPort)
	for _, field := range strings.Fields(strings.TrimSpace(string(out))) {
		if field == want {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) containerUsesImage(ctx context.Context, name, image string) (bool, error) {
	out, err := m.runDocker(ctx, dockerControlTimeout, "inspect", "-f", "{{.Config.Image}}", name)
	if containerMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == image, nil
}

func (m *Manager) ensureVolume(ctx context.Context, name string) error {
	_, err := m.runDocker(ctx, dockerControlTimeout, "volume", "inspect", name)
	if err == nil {
		return nil
	}
	if !volumeMissing(err) {
		return err
	}
	_, err = m.runDocker(ctx, dockerControlTimeout, "volume", "create", name)
	return err
}

func (m *Manager) ensureImage(ctx context.Context, image string) error {
	_, err := m.runDocker(ctx, dockerControlTimeout, "image", "inspect", image)
	if err == nil {
		return nil
	}
	if !imageMissing(err) {
		return err
	}
	_, err = m.runDocker(ctx, dockerDataTimeout, "pull", image)
	return err
}

func (m *Manager) ensureImagePlatform(ctx context.Context, image string) (string, error) {
	if image == "" {
		image = DefaultPostgresImage
	}
	if err := m.ensureImage(ctx, image); err != nil {
		return "", err
	}
	out, err := m.runDocker(ctx, dockerControlTimeout, "image", "inspect", "-f", "{{.Os}}/{{.Architecture}}", image)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

func containerMissing(err error) bool {
	return commandErrContains(err, "No such container") || commandErrContains(err, "No such object")
}

func volumeMissing(err error) bool {
	return commandErrContains(err, "No such volume") || commandErrContains(err, "No such object")
}

func imageMissing(err error) bool {
	return commandErrContains(err, "No such image") || commandErrContains(err, "No such object")
}

func commandErrContains(err error, fragment string) bool {
	if err == nil {
		return false
	}
	fragment = strings.ToLower(fragment)
	var outputErr *commandOutputError
	if errors.As(err, &outputErr) {
		return strings.Contains(strings.ToLower(string(outputErr.output)), fragment) || strings.Contains(strings.ToLower(outputErr.err.Error()), fragment)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.Contains(strings.ToLower(string(exitErr.Stderr)), fragment)
	}
	return strings.Contains(strings.ToLower(err.Error()), fragment)
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

func (m *Manager) runDocker(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	if timeout <= 0 {
		return m.runner.CombinedOutput(ctx, "docker", args...)
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := m.runner.CombinedOutput(commandCtx, "docker", args...)
	if err != nil && errors.Is(commandCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return out, fmt.Errorf("docker %s timed out after %s", strings.Join(args, " "), timeout)
	}
	return out, err
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

type execRunner struct{}

func (execRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	prepareRunnerCmd(cmd)
	cmd.Cancel = func() error {
		return killRunnerCmd(cmd)
	}
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, &commandOutputError{err: err, output: out}
	}
	return out, nil
}
