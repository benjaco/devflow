package database

import (
	"context"
	"errors"
	"fmt"
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
	DefaultPostgresImage = "postgres:16.3"
	DefaultSidecarImage  = "alpine:3.20"
	DefaultContainerPort = 5432
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
	ContainerName string    `json:"containerName"`
	VolumeName    string    `json:"volumeName"`
	Database      string    `json:"database"`
	User          string    `json:"user"`
	Port          int       `json:"port"`
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
		User:          cfg.User,
		Password:      cfg.Password,
		Image:         cfg.Image,
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
	if db.User == "" || db.Password == "" || db.Name == "" {
		return fmt.Errorf("database name, user, and password are required")
	}

	running, exists, err := m.inspectContainer(ctx, db.ContainerName)
	if err != nil {
		return err
	}
	if running {
		return nil
	}
	if err := m.ensureVolume(ctx, db.VolumeName); err != nil {
		return err
	}
	if exists {
		_, err := m.runner.CombinedOutput(ctx, "docker", "start", db.ContainerName)
		return err
	}
	_, err = m.runner.CombinedOutput(ctx, "docker", "run", "-d",
		"--name", db.ContainerName,
		"--label", "devflow.managed=true",
		"--label", "devflow.database=true",
		"-p", fmt.Sprintf("%d:%d", db.Port, DefaultContainerPort),
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
		_, err := m.runner.CombinedOutput(ctx, "docker", "exec", db.ContainerName, "pg_isready", "-U", db.User, "-d", db.Name)
		if err == nil {
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

func (m *Manager) StopRuntime(ctx context.Context, db api.DBInstance) error {
	if db.ContainerName == "" {
		return nil
	}
	_, err := m.runner.CombinedOutput(ctx, "docker", "stop", "-t", "10", db.ContainerName)
	if containerMissing(err) {
		return nil
	}
	return err
}

func (m *Manager) DestroyRuntime(ctx context.Context, db api.DBInstance, removeVolume bool) error {
	if db.ContainerName != "" {
		_, err := m.runner.CombinedOutput(ctx, "docker", "rm", "-f", db.ContainerName)
		if err != nil && !containerMissing(err) {
			return err
		}
	}
	if removeVolume && db.VolumeName != "" {
		_, err := m.runner.CombinedOutput(ctx, "docker", "volume", "rm", "-f", db.VolumeName)
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
	_, err := m.runner.CombinedOutput(ctx, "docker", "run", "--rm",
		"-v", db.VolumeName+":/from",
		"-v", snapshotDir+":/to",
		DefaultSidecarImage,
		"sh", "-c", "cd /from && tar czf /to/volume.tgz .",
	)
	if err != nil {
		return nil, err
	}
	manifest := &SnapshotManifest{
		Version:       1,
		Key:           key,
		CreatedAt:     time.Now().UTC(),
		Image:         db.Image,
		ContainerName: db.ContainerName,
		VolumeName:    db.VolumeName,
		Database:      db.Name,
		User:          db.User,
		Port:          db.Port,
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
	if err := m.DestroyRuntime(ctx, db, true); err != nil {
		return nil, err
	}
	if err := m.ensureVolume(ctx, db.VolumeName); err != nil {
		return nil, err
	}
	_, err = m.runner.CombinedOutput(ctx, "docker", "run", "--rm",
		"-v", db.VolumeName+":/to",
		"-v", snapshotDir+":/from",
		DefaultSidecarImage,
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
	out, inspectErr := m.runner.CombinedOutput(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name)
	if inspectErr != nil {
		if containerMissing(inspectErr) {
			return false, false, nil
		}
		return false, false, inspectErr
	}
	return strings.TrimSpace(string(out)) == "true", true, nil
}

func (m *Manager) ensureVolume(ctx context.Context, name string) error {
	_, err := m.runner.CombinedOutput(ctx, "docker", "volume", "inspect", name)
	if err == nil {
		return nil
	}
	if !volumeMissing(err) {
		return err
	}
	_, err = m.runner.CombinedOutput(ctx, "docker", "volume", "create", name)
	return err
}

func postgresURL(host string, port int, user, password, database string) string {
	auth := user
	if password != "" {
		auth += ":" + password
	}
	if port > 0 {
		return "postgres://" + auth + "@" + host + ":" + strconv.Itoa(port) + "/" + database + "?sslmode=disable"
	}
	return "postgres://" + auth + "@" + host + "/" + database + "?sslmode=disable"
}

func containerMissing(err error) bool {
	return commandErrContains(err, "No such container") || commandErrContains(err, "No such object")
}

func volumeMissing(err error) bool {
	return commandErrContains(err, "No such volume") || commandErrContains(err, "No such object")
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

type execRunner struct{}

func (execRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, &commandOutputError{err: err, output: out}
	}
	return out, nil
}
