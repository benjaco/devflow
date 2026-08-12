package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestDesiredBuildsDedicatedInstanceIdentity(t *testing.T) {
	mgr := New()
	db := mgr.Desired("abc123", Config{
		HostPort:     55432,
		Database:     "app_wt_abc123",
		User:         "devflow",
		Password:     "secret",
		SnapshotRoot: "/tmp/snapshots",
	})
	if db.ContainerName != "devflow-pg-abc123" {
		t.Fatalf("unexpected container name: %q", db.ContainerName)
	}
	if db.VolumeName != "devflow-pgdata-abc123" {
		t.Fatalf("unexpected volume name: %q", db.VolumeName)
	}
	if db.Port != 55432 {
		t.Fatalf("unexpected port: %d", db.Port)
	}
	if db.ContainerPort != DefaultContainerPort {
		t.Fatalf("unexpected container port: %d", db.ContainerPort)
	}
	if db.SidecarImage != DefaultSidecarImage {
		t.Fatalf("unexpected sidecar image: %q", db.SidecarImage)
	}
	if db.Flavor != FlavorPostgres {
		t.Fatalf("unexpected default database flavor: %q", db.Flavor)
	}
	if !strings.Contains(db.URL, "@127.0.0.1:55432/app_wt_abc123?sslmode=disable") {
		t.Fatalf("unexpected database URL: %q", db.URL)
	}
}

func TestDesiredRejectsUnsupportedDatabaseFlavor(t *testing.T) {
	db := New().Desired("custom", Config{Flavor: "unknown", HostPort: 55432})
	err := newTestManager(&fakeRunner{}).EnsureRuntime(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), `unsupported database flavor "unknown"`) {
		t.Fatalf("expected unsupported flavor error, got %v", err)
	}
}

func TestEnsureRuntimeRejectsUnsupportedPostGISVersionBeforeDocker(t *testing.T) {
	db := New().Desired("geo", Config{
		Flavor:          FlavorPostGIS,
		PostgresVersion: 15,
		HostPort:        55432,
	})
	runner := &fakeRunner{}
	err := newTestManager(runner).EnsureRuntime(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "supported versions are 16, 17, and 18") {
		t.Fatalf("expected unsupported PostGIS version error, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected version validation before Docker calls, got %+v", runner.calls)
	}
}

func TestDesiredPreservesCustomRuntimeImagesAndContainerPort(t *testing.T) {
	mgr := New()
	db := mgr.Desired("custom", Config{
		Image:         "example/postgres:arm-ready",
		SidecarImage:  "example/tar:stable",
		HostPort:      55432,
		ContainerPort: 6432,
	})
	if db.Image != "example/postgres:arm-ready" {
		t.Fatalf("unexpected Postgres image: %q", db.Image)
	}
	if db.SidecarImage != "example/tar:stable" {
		t.Fatalf("unexpected sidecar image: %q", db.SidecarImage)
	}
	if db.ContainerPort != 6432 {
		t.Fatalf("unexpected container port: %d", db.ContainerPort)
	}
}

func TestDesiredPostGISPreservesFlavorForRuntimeResolution(t *testing.T) {
	mgr := New()
	db := mgr.Desired("geo", Config{
		Flavor:          FlavorPostGIS,
		PostgresVersion: 17,
		HostPort:        55432,
		Database:        "geo",
	})
	if db.Flavor != FlavorPostGIS {
		t.Fatalf("unexpected database flavor %q", db.Flavor)
	}
	if db.Image != "" {
		t.Fatalf("expected PostGIS image selection to be deferred until Docker architecture is known, got %q", db.Image)
	}
	if db.PostgresVersion != 17 {
		t.Fatalf("unexpected PostgreSQL version %d", db.PostgresVersion)
	}
	if db.VolumeName != "devflow-pgdata-geo-pg17" {
		t.Fatalf("unexpected versioned PostGIS volume %q", db.VolumeName)
	}
}

func TestPostGISRuntimeForVersionsAndArchitectures(t *testing.T) {
	versions := []struct {
		postgres int
		postGIS  string
	}{
		{postgres: 16, postGIS: "3.5"},
		{postgres: 17, postGIS: "3.5"},
		{postgres: 18, postGIS: "3.6"},
	}
	for _, version := range versions {
		t.Run(strconv.Itoa(version.postgres), func(t *testing.T) {
			runtime, err := postGISRuntimeForVersion(version.postgres)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.upstreamPostGISVersion != version.postGIS {
				t.Fatalf("unexpected upstream PostGIS version %q, want %q", runtime.upstreamPostGISVersion, version.postGIS)
			}
			if runtime.arm64BaseImage != fmt.Sprintf("postgres:%d-bookworm", version.postgres) {
				t.Fatalf("unexpected arm64 base image %q", runtime.arm64BaseImage)
			}
			for _, test := range []struct {
				arch string
				want string
			}{
				{arch: "amd64", want: fmt.Sprintf("postgis/postgis:%d-%s", version.postgres, version.postGIS)},
				{arch: "x86_64", want: fmt.Sprintf("postgis/postgis:%d-%s", version.postgres, version.postGIS)},
				{arch: "arm64", want: fmt.Sprintf("devflow/postgis:%d-bookworm-postgis3-arm64-v1", version.postgres)},
				{arch: "aarch64", want: fmt.Sprintf("devflow/postgis:%d-bookworm-postgis3-arm64-v1", version.postgres)},
			} {
				got, err := postGISImageForArchitecture(version.postgres, test.arch)
				if err != nil {
					t.Fatal(err)
				}
				if got != test.want {
					t.Fatalf("unexpected image %q for %s, want %q", got, test.arch, test.want)
				}
			}
		})
	}
	for _, version := range []int{0, 15, 19} {
		if _, err := postGISRuntimeForVersion(version); err == nil {
			t.Fatalf("expected PostgreSQL version %d to fail explicitly", version)
		}
	}
	if _, err := postGISImageForArchitecture(16, "riscv64"); err == nil {
		t.Fatal("expected unsupported Docker architecture to fail explicitly")
	}
	dockerfile := string(postGISARMDockerfile)
	for _, required := range []string{
		"ARG POSTGRES_MAJOR",
		"FROM postgres:${POSTGRES_MAJOR}-bookworm",
		`"postgresql-${POSTGRES_MAJOR}-postgis-3"`,
		`"postgresql-${POSTGRES_MAJOR}-postgis-3-scripts"`,
		"rm -rf /var/lib/apt/lists/*",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Fatalf("embedded PostGIS Dockerfile is missing %q:\n%s", required, dockerfile)
		}
	}
}

func TestPostgresVolumeMountUsesVersion18Layout(t *testing.T) {
	for _, test := range []struct {
		version int
		want    string
	}{
		{version: 16, want: postgresDataMount},
		{version: 17, want: postgresDataMount},
		{version: 18, want: postgres18DataMount},
	} {
		if got := postgresVolumeMount(api.DBInstance{PostgresVersion: test.version}); got != test.want {
			t.Fatalf("PostgreSQL %d mount = %q, want %q", test.version, got, test.want)
		}
	}
}

func TestEnsureRuntimePullsMaintainedPostGISImageOnAMD64(t *testing.T) {
	postGISRuntime, err := postGISRuntimeForVersion(17)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "info", "--format", "{{.Architecture}}"):                 {out: []byte("x86_64\n")},
			key("docker", "image", "inspect", postGISRuntime.amd64Image):           {err: errors.New("Error response from daemon: No such image: " + postGISRuntime.amd64Image)},
			key("docker", "pull", postGISRuntime.amd64Image):                       {},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-geo"): {err: errors.New("Error: No such container: devflow-pg-geo")},
			key("docker", "volume", "inspect", "devflow-pgdata-geo"):               {err: errors.New("Error: No such volume: devflow-pgdata-geo")},
			key("docker", "volume", "create", "devflow-pgdata-geo"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-geo", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=geo", "-v", "devflow-pgdata-geo:/var/lib/postgresql/data", postGISRuntime.amd64Image): {},
		},
	}
	db := api.DBInstance{
		Name:            "geo",
		Port:            55432,
		User:            "devflow",
		Password:        "secret",
		Flavor:          FlavorPostGIS,
		PostgresVersion: 17,
		ContainerName:   "devflow-pg-geo",
		VolumeName:      "devflow-pgdata-geo",
	}
	if err := newTestManager(runner).EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.calledBefore("docker pull "+postGISRuntime.amd64Image, "docker run -d --name devflow-pg-geo") {
		t.Fatalf("expected maintained PostGIS image pull before container start, calls: %+v", runner.calls)
	}
	if runner.sawPrefix("docker build ") {
		t.Fatalf("did not expect an amd64 PostGIS image build, calls: %+v", runner.calls)
	}
}

func TestEnsureRuntimeBuildsNativePostGISImageOnARM64(t *testing.T) {
	postGISRuntime, err := postGISRuntimeForVersion(18)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "info", "--format", "{{.Architecture}}"): {out: []byte("aarch64\n")},
			key("docker", "image", "inspect", "-f", "{{.Architecture}}", postGISRuntime.arm64Image): {sequence: []response{
				{err: errors.New("Error response from daemon: No such image: " + postGISRuntime.arm64Image)},
				{out: []byte("arm64\n")},
			}},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-geo"): {err: errors.New("Error: No such container: devflow-pg-geo")},
			key("docker", "volume", "inspect", "devflow-pgdata-geo"):               {err: errors.New("Error: No such volume: devflow-pgdata-geo")},
			key("docker", "volume", "create", "devflow-pgdata-geo"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-geo", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=geo", "-v", "devflow-pgdata-geo:/var/lib/postgresql", postGISRuntime.arm64Image): {},
		},
	}
	db := api.DBInstance{
		Name:            "geo",
		Port:            55432,
		User:            "devflow",
		Password:        "secret",
		Flavor:          FlavorPostGIS,
		PostgresVersion: 18,
		ContainerName:   "devflow-pg-geo",
		VolumeName:      "devflow-pgdata-geo",
	}
	if err := newTestManager(runner).EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	buildPrefix := "docker build --pull --platform linux/arm64 --build-arg POSTGRES_MAJOR=18 --label devflow.managed=true --label devflow.database=true -t " + postGISRuntime.arm64Image
	buildCall := runner.callWithPrefix(buildPrefix)
	if buildCall == "" {
		t.Fatalf("expected native arm64 PostGIS image build, calls: %+v", runner.calls)
	}
	if len(runner.builds) != 1 {
		t.Fatalf("expected one structured Engine API build, got %+v", runner.builds)
	}
	build := runner.builds[0]
	if build.Tag != postGISRuntime.arm64Image || build.Platform != "linux/arm64" || build.BuildArgs["POSTGRES_MAJOR"] != "18" {
		t.Fatalf("unexpected structured PostGIS build request: %+v", build)
	}
	if string(build.Dockerfile) != string(postGISARMDockerfile) {
		t.Fatal("expected the embedded PostGIS Dockerfile in the Engine API build context")
	}
	if !runner.calledBefore("docker build ", "docker run -d --name devflow-pg-geo") {
		t.Fatalf("expected PostGIS build before container start, calls: %+v", runner.calls)
	}
	if runner.sawPrefix("docker pull " + postGISRuntime.arm64Image) {
		t.Fatalf("did not expect the local arm64 image to be pulled, calls: %+v", runner.calls)
	}
}

func TestEnsureRuntimeReusesCachedNativePostGISImageOnARM64(t *testing.T) {
	postGISRuntime, err := postGISRuntimeForVersion(16)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "info", "--format", "{{.Architecture}}"):                                  {out: []byte("arm64\n")},
			key("docker", "image", "inspect", "-f", "{{.Architecture}}", postGISRuntime.arm64Image): {out: []byte("arm64\n")},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-geo"):                  {err: errors.New("Error: No such container: devflow-pg-geo")},
			key("docker", "volume", "inspect", "devflow-pgdata-geo"):                                {},
			key("docker", "run", "-d", "--name", "devflow-pg-geo", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=geo", "-v", "devflow-pgdata-geo:/var/lib/postgresql/data", postGISRuntime.arm64Image): {},
		},
	}
	db := api.DBInstance{
		Name:            "geo",
		Port:            55432,
		User:            "devflow",
		Password:        "secret",
		Flavor:          FlavorPostGIS,
		PostgresVersion: 16,
		ContainerName:   "devflow-pg-geo",
		VolumeName:      "devflow-pgdata-geo",
	}
	if err := newTestManager(runner).EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if runner.sawPrefix("docker build ") || runner.sawPrefix("docker pull ") {
		t.Fatalf("expected cached native PostGIS image reuse, calls: %+v", runner.calls)
	}
}

func TestEnsureRuntimeRebuildsWrongArchitecturePostGISCacheOnARM64(t *testing.T) {
	postGISRuntime, err := postGISRuntimeForVersion(17)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string]response{
		key("docker", "info", "--format", "{{.Architecture}}"): {out: []byte("arm64\n")},
		key("docker", "image", "inspect", "-f", "{{.Architecture}}", postGISRuntime.arm64Image): {sequence: []response{
			{out: []byte("amd64\n")},
			{out: []byte("arm64\n")},
		}},
		key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-geo"): {err: errors.New("Error: No such container: devflow-pg-geo")},
		key("docker", "volume", "inspect", "devflow-pgdata-geo"):               {},
		key("docker", "run", "-d", "--name", "devflow-pg-geo", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=geo", "-v", "devflow-pgdata-geo:/var/lib/postgresql/data", postGISRuntime.arm64Image): {},
	}}
	db := api.DBInstance{
		Name:            "geo",
		Port:            55432,
		User:            "devflow",
		Password:        "secret",
		Flavor:          FlavorPostGIS,
		PostgresVersion: 17,
		ContainerName:   "devflow-pg-geo",
		VolumeName:      "devflow-pgdata-geo",
	}
	if err := newTestManager(runner).EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker build --pull --platform linux/arm64") {
		t.Fatalf("expected wrong-architecture cache to be rebuilt, calls: %+v", runner.calls)
	}
}

func TestEnsureRuntimePostGISCustomImageOverridesArchitectureSelection(t *testing.T) {
	runner := &fakeRunner{responses: map[string]response{
		key("docker", "image", "inspect", "example/postgis:multiarch"):         {},
		key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-geo"): {err: errors.New("Error: No such container: devflow-pg-geo")},
		key("docker", "volume", "inspect", "devflow-pgdata-geo"):               {},
		key("docker", "run", "-d", "--name", "devflow-pg-geo", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=geo", "-v", "devflow-pgdata-geo:/var/lib/postgresql", "example/postgis:multiarch"): {},
	}}
	db := api.DBInstance{
		Name:            "geo",
		Port:            55432,
		User:            "devflow",
		Password:        "secret",
		Flavor:          FlavorPostGIS,
		PostgresVersion: 18,
		Image:           "example/postgis:multiarch",
		ContainerName:   "devflow-pg-geo",
		VolumeName:      "devflow-pgdata-geo",
	}
	if err := newTestManager(runner).EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if runner.sawPrefix("docker info ") || runner.sawPrefix("docker build ") {
		t.Fatalf("expected explicit PostGIS image to bypass architecture selection, calls: %+v", runner.calls)
	}
}

func TestWaitReadyEnablesPostGISExtension(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	runner := &fakeRunner{responses: map[string]response{
		key("docker", "exec", "devflow-pg-geo", "pg_isready", "-U", "devflow", "-d", "geo"):                                                                          {},
		key("docker", "exec", "devflow-pg-geo", "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "devflow", "-d", "geo", "-c", "CREATE EXTENSION IF NOT EXISTS postgis"): {},
	}}
	db := api.DBInstance{
		Name:          "geo",
		Host:          "127.0.0.1",
		Port:          port,
		User:          "devflow",
		Flavor:        FlavorPostGIS,
		ContainerName: "devflow-pg-geo",
	}
	if err := newTestManager(runner).WaitReady(context.Background(), db, time.Second); err != nil {
		t.Fatal(err)
	}
	extensionCommand := key("docker", "exec", "devflow-pg-geo", "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "devflow", "-d", "geo", "-c", "CREATE EXTENSION IF NOT EXISTS postgis")
	if runner.callCount(extensionCommand) != 1 {
		t.Fatalf("expected PostGIS extension activation, calls: %+v", runner.calls)
	}
}

func TestWaitReadyReportsPostGISExtensionFailureAtReadinessTimeout(t *testing.T) {
	runner := &fakeRunner{responses: map[string]response{
		key("docker", "exec", "devflow-pg-geo", "pg_isready", "-U", "devflow", "-d", "geo"):                                                                          {},
		key("docker", "exec", "devflow-pg-geo", "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "devflow", "-d", "geo", "-c", "CREATE EXTENSION IF NOT EXISTS postgis"): {err: errors.New("extension postgis is not available")},
	}}
	db := api.DBInstance{
		Name:          "geo",
		Port:          55432,
		User:          "devflow",
		Flavor:        FlavorPostGIS,
		ContainerName: "devflow-pg-geo",
	}
	err := newTestManager(runner).WaitReady(context.Background(), db, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), "extension postgis is not available") {
		t.Fatalf("expected bounded PostGIS readiness failure, got %v", err)
	}
}

func TestExecSQLUsesManagedContainerEngineAPI(t *testing.T) {
	statement := "CREATE TABLE widgets (id integer PRIMARY KEY);"
	command := key(
		"docker", "exec", "devflow-pg-app",
		"psql", "-X", "-v", "ON_ERROR_STOP=1",
		"-U", "devflow", "-d", "app", "-c", statement,
	)
	runner := &fakeRunner{responses: map[string]response{
		command: {out: []byte("CREATE TABLE\n")},
	}}
	output, err := newTestManager(runner).ExecSQL(context.Background(), api.DBInstance{
		Name:          "app",
		User:          "devflow",
		ContainerName: "devflow-pg-app",
	}, statement)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "CREATE TABLE\n" {
		t.Fatalf("SQL output = %q", output)
	}
	if len(runner.calls) != 1 || runner.calls[0] != command {
		t.Fatalf("unexpected Engine exec planning calls: %+v", runner.calls)
	}
}

func TestExecSQLValidatesDatabaseIdentityBeforeEngineCall(t *testing.T) {
	runner := &fakeRunner{}
	_, err := newTestManager(runner).ExecSQL(context.Background(), api.DBInstance{}, "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "container name is required") {
		t.Fatalf("expected database identity error, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("validation reached the Engine boundary: %+v", runner.calls)
	}
}

func TestExecSQLUsesConfiguredContainerPort(t *testing.T) {
	statement := "SELECT 1"
	command := key(
		"docker", "exec", "devflow-pg-custom",
		"psql", "-X", "-v", "ON_ERROR_STOP=1",
		"-U", "devflow", "-d", "app", "-p", "6432", "-c", statement,
	)
	runner := &fakeRunner{responses: map[string]response{command: {}}}
	_, err := newTestManager(runner).ExecSQL(context.Background(), api.DBInstance{
		Name:          "app",
		User:          "devflow",
		ContainerName: "devflow-pg-custom",
		ContainerPort: 6432,
	}, statement)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != command {
		t.Fatalf("configured container port was not passed to psql: %+v", runner.calls)
	}
}

func TestEnsureRuntimeCreatesVolumeAndContainer(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "create", "devflow-pgdata-abc"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.14"): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker volume create devflow-pgdata-abc") {
		t.Fatal("expected docker volume create")
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected docker run for container start")
	}
}

func TestEnsureRuntimePullsMissingImageBeforeColdContainerStart(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-cold"): {err: errors.New("Error: No such container: devflow-pg-cold")},
			key("docker", "image", "inspect", DefaultPostgresImage):                 {err: errors.New("Error response from daemon: No such image: " + DefaultPostgresImage)},
			key("docker", "pull", DefaultPostgresImage):                             {},
			key("docker", "volume", "inspect", "devflow-pgdata-cold"):               {err: errors.New("Error: No such volume: devflow-pgdata-cold")},
			key("docker", "volume", "create", "devflow-pgdata-cold"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-cold", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_cold", "-v", "devflow-pgdata-cold:/var/lib/postgresql/data", DefaultPostgresImage): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_cold",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-cold",
		VolumeName:    "devflow-pgdata-cold",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker pull " + DefaultPostgresImage) {
		t.Fatal("expected missing image to be pulled before container start")
	}
	if !runner.calledBefore("docker pull "+DefaultPostgresImage, "docker run -d --name devflow-pg-cold") {
		t.Fatalf("expected image pull before container start, calls: %+v", runner.calls)
	}
}

func TestEnsureRuntimeReusesRunningContainerWithExpectedHostPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55432\n")},
			imageInspectKey("devflow-pg-abc"):                                      {out: []byte(DefaultPostgresImage + "\n")},
			volumeInspectKey("devflow-pg-abc"):                                     {out: []byte("volume devflow-pgdata-abc /var/lib/postgresql/data\n")},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected existing container to be reused")
	}
}

func TestEnsureRuntimeStartsStoppedContainerWithExpectedHostPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("false\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55432\n")},
			imageInspectKey("devflow-pg-abc"):                                      {out: []byte(DefaultPostgresImage + "\n")},
			volumeInspectKey("devflow-pg-abc"):                                     {out: []byte("volume devflow-pgdata-abc /var/lib/postgresql/data\n")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {},
			key("docker", "start", "devflow-pg-abc"):                               {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker start devflow-pg-abc") {
		t.Fatal("expected stopped container to be started")
	}
	if runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("did not expect replacement container when published port matches")
	}
}

func TestEnsureRuntimeRecreatesContainerWithWrongImage(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55432\n")},
			imageInspectKey("devflow-pg-abc"):                                      {out: []byte("postgres:15.10\n")},
			volumeInspectKey("devflow-pg-abc"):                                     {out: []byte("volume devflow-pgdata-abc /var/lib/postgresql/data\n")},
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", DefaultPostgresImage): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker rm -f devflow-pg-abc") {
		t.Fatal("expected container using the stale image to be removed")
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected container using the configured image to be started")
	}
}

func TestEnsureRuntimeHonorsCustomContainerPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-custom"): {err: errors.New("Error: No such container: devflow-pg-custom")},
			key("docker", "volume", "inspect", "devflow-pgdata-custom"):               {err: errors.New("Error: No such volume: devflow-pgdata-custom")},
			key("docker", "volume", "create", "devflow-pgdata-custom"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-custom", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:6432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_custom", "-v", "devflow-pgdata-custom:/var/lib/postgresql/data", "example/postgres:custom"): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_custom",
		Port:          55432,
		ContainerPort: 6432,
		User:          "devflow",
		Password:      "secret",
		Image:         "example/postgres:custom",
		ContainerName: "devflow-pg-custom",
		VolumeName:    "devflow-pgdata-custom",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-custom") {
		t.Fatal("expected custom-port container start")
	}
}

func TestEnsureRuntimeRecreatesPostgres18ContainerWithLegacyVolumeDestination(t *testing.T) {
	postGISRuntime, err := postGISRuntimeForVersion(18)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string]response{
		key("docker", "info", "--format", "{{.Architecture}}"):                 {out: []byte("amd64\n")},
		key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-geo"): {out: []byte("true\n")},
		portInspectKey("devflow-pg-geo"):                                       {out: []byte("55432\n")},
		imageInspectKey("devflow-pg-geo"):                                      {out: []byte(postGISRuntime.amd64Image + "\n")},
		volumeInspectKey("devflow-pg-geo"):                                     {out: []byte("volume devflow-pgdata-geo-pg18 /var/lib/postgresql/data\n")},
		key("docker", "image", "inspect", postGISRuntime.amd64Image):           {},
		key("docker", "rm", "-f", "devflow-pg-geo"):                            {},
		key("docker", "volume", "inspect", "devflow-pgdata-geo-pg18"):          {},
		key("docker", "run", "-d", "--name", "devflow-pg-geo", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=geo", "-v", "devflow-pgdata-geo-pg18:/var/lib/postgresql", postGISRuntime.amd64Image): {},
	}}
	db := api.DBInstance{
		Name:            "geo",
		Port:            55432,
		User:            "devflow",
		Password:        "secret",
		Flavor:          FlavorPostGIS,
		PostgresVersion: 18,
		ContainerName:   "devflow-pg-geo",
		VolumeName:      "devflow-pgdata-geo-pg18",
	}
	if err := newTestManager(runner).EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker rm -f devflow-pg-geo") {
		t.Fatalf("expected legacy PostgreSQL 18 mount to be reconciled, calls: %+v", runner.calls)
	}
}

func TestEnsureRuntimeRecreatesContainerWithWrongHostPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55433\n")},
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.14"): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker rm -f devflow-pg-abc") {
		t.Fatal("expected stale container to be removed")
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected replacement container to be started")
	}
}

func TestEnsureRuntimeTimesOutStaleContainerRemoval(t *testing.T) {
	oldTimeout := dockerControlTimeout
	dockerControlTimeout = 20 * time.Millisecond
	defer func() { dockerControlTimeout = oldTimeout }()

	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55433\n")},
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {waitContext: true},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	err := mgr.EnsureRuntime(context.Background(), db)
	if err == nil {
		t.Fatal("expected timeout removing stale container")
	}
	if !strings.Contains(err.Error(), "remove Docker container devflow-pg-abc timed out after") {
		t.Fatalf("expected Docker Engine timeout error, got %v", err)
	}
}

func TestWaitReadyAlsoWaitsForHostPortWhenHostSet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Host:          "127.0.0.1",
		Port:          port,
		User:          "devflow",
		ContainerName: "devflow-pg-abc",
	}
	if err := mgr.WaitReady(context.Background(), db, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyRetriesUntilConfiguredDatabaseAcceptsSQL(t *testing.T) {
	probeCommand := key(
		"docker", "exec", "devflow-pg-abc",
		"psql", "-X", "-v", "ON_ERROR_STOP=1",
		"-h", "127.0.0.1", "-U", "devflow", "-d", "app_wt_abc", "-At", "-c", "SELECT 1",
	)
	runner := &fakeRunner{responses: map[string]response{
		key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
		probeCommand: {sequence: []response{
			{err: errors.New(`FATAL: database "app_wt_abc" does not exist`)},
			{},
		}},
	}}
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		ContainerName: "devflow-pg-abc",
	}
	if err := newTestManager(runner).WaitReady(context.Background(), db, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := runner.callCount(probeCommand); got != 2 {
		t.Fatalf("database SQL readiness probes = %d, want 2; calls: %+v", got, runner.calls)
	}
}

func TestWaitReadyUsesCustomContainerPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "exec", "devflow-pg-custom", "pg_isready", "-U", "devflow", "-d", "app_wt_custom", "-p", "6432"): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_custom",
		Host:          "127.0.0.1",
		Port:          port,
		ContainerPort: 6432,
		User:          "devflow",
		ContainerName: "devflow-pg-custom",
	}
	if err := mgr.WaitReady(context.Background(), db, time.Second); err != nil {
		t.Fatal(err)
	}
	probeCommand := key(
		"docker", "exec", "devflow-pg-custom",
		"psql", "-X", "-v", "ON_ERROR_STOP=1",
		"-h", "127.0.0.1", "-U", "devflow", "-d", "app_wt_custom", "-p", "6432", "-At", "-c", "SELECT 1",
	)
	if runner.callCount(probeCommand) != 1 {
		t.Fatalf("configured container port was not passed to SQL readiness probe: %+v", runner.calls)
	}
}

func TestWaitHostReadyReportsHostPortFailures(t *testing.T) {
	mgr := newTestManager(&fakeRunner{})
	db := api.DBInstance{Host: "127.0.0.1", Port: freeLocalPort(t)}
	err := mgr.WaitHostReady(context.Background(), db, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "database host port") {
		t.Fatalf("expected host readiness failure, got %v", err)
	}
}

func TestSnapshotWritesManifestAndArchiveCommand(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "stop", "-t", "10", "devflow-pg-abc"): {},
			platformInspectKey(DefaultPostgresImage):            {out: []byte("linux/arm64\n")},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(root, "schema_v1")+":/to", "example/tar:stable", "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		Image:         "postgres:16.14",
		SidecarImage:  "example/tar:stable",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		SnapshotRoot:  root,
	}
	manifest, err := mgr.Snapshot(context.Background(), db, "schema_v1")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Key != "schema_v1" {
		t.Fatalf("unexpected snapshot key: %q", manifest.Key)
	}
	if manifest.Version != 3 || manifest.Platform != "linux/arm64" || manifest.SidecarImage != "example/tar:stable" || manifest.ContainerPort != DefaultContainerPort {
		t.Fatalf("unexpected snapshot runtime metadata: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(root, "schema_v1", "manifest.json")); err != nil {
		t.Fatalf("expected manifest to exist: %v", err)
	}
	if !runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("expected docker stop before snapshot")
	}
}

func TestRestoreSnapshotRecreatesVolumeAndUntars(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "schema_v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := SnapshotManifest{
		Version:       1,
		Key:           "schema_v1",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(root, "schema_v1", "volume.tgz"),
	}
	if err := os.WriteFile(filepath.Join(root, "schema_v1", "volume.tgz"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := jsonWrite(filepath.Join(root, "schema_v1", "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):  {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "create", "devflow-pgdata-abc"):   {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/to", "-v", filepath.Join(root, "schema_v1")+":/from", DefaultSidecarImage, "sh", "-c", "cd /to && tar xzf /from/volume.tgz"): {},
		},
	}
	mgr := newTestManager(runner)
	db := api.DBInstance{
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		SnapshotRoot:  root,
	}
	got, err := mgr.RestoreSnapshot(context.Background(), db, "schema_v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "schema_v1" {
		t.Fatalf("unexpected restored manifest key: %q", got.Key)
	}
	if !runner.sawPrefix("docker volume create devflow-pgdata-abc") {
		t.Fatal("expected docker volume create during restore")
	}
}

func TestRestoreNearestPrismaSnapshotTreatsUnknownOrMismatchedPlatformAsCacheMiss(t *testing.T) {
	for _, snapshotPlatform := range []string{"", "linux/amd64"} {
		name := snapshotPlatform
		if name == "" {
			name = "legacy-unknown"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			state := &PrismaState{
				SchemaHash: "schema",
				Migrations: []PrismaMigration{{Name: "001_init", Hash: "hash"}},
				FullHash:   "full",
			}
			if _, err := SavePrismaSnapshot(root, "snapshot", state); err != nil {
				t.Fatal(err)
			}
			manifest := SnapshotManifest{
				Version:       2,
				Key:           "snapshot",
				Image:         DefaultPostgresImage,
				Platform:      snapshotPlatform,
				ContainerName: "devflow-pg-abc",
				VolumeName:    "devflow-pgdata-abc",
				Database:      "app_wt_abc",
				User:          "devflow",
				Port:          55432,
				ArchivePath:   filepath.Join(root, "snapshot", "volume.tgz"),
			}
			if err := jsonWrite(filepath.Join(root, "snapshot", "manifest.json"), manifest); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{responses: map[string]response{
				platformInspectKey(DefaultPostgresImage): {out: []byte("linux/arm64\n")},
			}}
			mgr := newTestManager(runner)
			db := api.DBInstance{
				Image:         DefaultPostgresImage,
				ContainerName: "devflow-pg-abc",
				VolumeName:    "devflow-pgdata-abc",
				SnapshotRoot:  root,
			}
			result, err := mgr.RestoreNearestPrismaSnapshot(context.Background(), db, state)
			if err != nil {
				t.Fatal(err)
			}
			if result != nil {
				t.Fatalf("expected incompatible snapshot to be ignored, got %+v", result)
			}
			if runner.sawPrefix("docker rm -f") || runner.sawPrefix("docker volume rm -f") {
				t.Fatalf("expected compatibility check before destructive restore, calls: %+v", runner.calls)
			}
		})
	}
}

func TestRestoreNearestPrismaSnapshotTreatsMismatchedRuntimeImageAsCacheMiss(t *testing.T) {
	postGISRuntime, err := postGISRuntimeForVersion(16)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := &PrismaState{
		SchemaHash: "schema",
		Migrations: []PrismaMigration{{Name: "001_init", Hash: "hash"}},
		FullHash:   "full",
	}
	if _, err := SavePrismaSnapshot(root, "snapshot", state); err != nil {
		t.Fatal(err)
	}
	manifest := SnapshotManifest{
		Version:       2,
		Key:           "snapshot",
		Image:         DefaultPostgresImage,
		Platform:      "linux/amd64",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(root, "snapshot", "volume.tgz"),
	}
	if err := jsonWrite(filepath.Join(root, "snapshot", "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string]response{
		key("docker", "info", "--format", "{{.Architecture}}"): {out: []byte("amd64\n")},
		platformInspectKey(postGISRuntime.amd64Image):          {out: []byte("linux/amd64\n")},
	}}
	db := api.DBInstance{
		Flavor:          FlavorPostGIS,
		PostgresVersion: 16,
		ContainerName:   "devflow-pg-abc",
		VolumeName:      "devflow-pgdata-abc",
		SnapshotRoot:    root,
	}
	result, err := newTestManager(runner).RestoreNearestPrismaSnapshot(context.Background(), db, state)
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Fatalf("expected cross-flavor snapshot to be ignored, got %+v", result)
	}
	if runner.sawPrefix("docker rm -f") || runner.sawPrefix("docker volume rm -f") {
		t.Fatalf("expected image compatibility check before destructive restore, calls: %+v", runner.calls)
	}
}

func TestRestoreSnapshotRejectsMissingOrMismatchedPostgresVersionBeforeDestructiveWork(t *testing.T) {
	for _, snapshotVersion := range []int{0, 16} {
		t.Run(strconv.Itoa(snapshotVersion), func(t *testing.T) {
			root := t.TempDir()
			keyName := "versioned"
			if err := os.MkdirAll(filepath.Join(root, keyName), 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := SnapshotManifest{
				Version:         3,
				Key:             keyName,
				Image:           "example/postgis:multiarch",
				Platform:        "linux/amd64",
				PostgresVersion: snapshotVersion,
				ContainerName:   "devflow-pg-geo",
				VolumeName:      "devflow-pgdata-geo-pg17",
				ArchivePath:     filepath.Join(root, keyName, "volume.tgz"),
			}
			if err := jsonWrite(filepath.Join(root, keyName, "manifest.json"), manifest); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{responses: map[string]response{
				key("docker", "image", "inspect", "example/postgis:multiarch"): {},
				platformInspectKey("example/postgis:multiarch"):                {out: []byte("linux/amd64\n")},
			}}
			db := api.DBInstance{
				Flavor:          FlavorPostGIS,
				PostgresVersion: 17,
				Image:           "example/postgis:multiarch",
				ContainerName:   "devflow-pg-geo",
				VolumeName:      "devflow-pgdata-geo-pg17",
				SnapshotRoot:    root,
			}
			_, err := newTestManager(runner).RestoreSnapshot(context.Background(), db, keyName)
			if err == nil || !errors.Is(err, ErrSnapshotIncompatible) || !strings.Contains(err.Error(), "PostgreSQL") {
				t.Fatalf("expected PostgreSQL-version incompatibility, got %v", err)
			}
			if runner.sawPrefix("docker rm -f") || runner.sawPrefix("docker volume rm -f") {
				t.Fatalf("expected version check before destructive restore, calls: %+v", runner.calls)
			}
		})
	}
}

func TestSnapshotKeySkipsEmptyParts(t *testing.T) {
	if got := SnapshotKey("db", "", "schema", "v1"); got != "db_schema_v1" {
		t.Fatalf("unexpected snapshot key %q", got)
	}
}

func TestDatabaseSnapshotKeyRejectsPathTraversalBeforeDockerCalls(t *testing.T) {
	runner := &fakeRunner{}
	db := api.DBInstance{SnapshotRoot: t.TempDir()}
	for _, invalid := range []string{"", ".", "..", "../outside", `..\outside`, "nested/child", `nested\child`} {
		t.Run(strings.ReplaceAll(invalid, "/", "_"), func(t *testing.T) {
			if _, err := newTestManager(runner).Snapshot(context.Background(), db, invalid); err == nil {
				t.Fatalf("expected snapshot key %q to be rejected", invalid)
			}
		})
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid snapshot keys must fail before Docker calls, got %+v", runner.calls)
	}
	if got, err := databaseSnapshotDir(db.SnapshotRoot, "schema_v1-02.hash"); err != nil || got != filepath.Join(db.SnapshotRoot, "schema_v1-02.hash") {
		t.Fatalf("portable snapshot key rejected: path=%q err=%v", got, err)
	}
}

func TestPostgresURLEscapesCredentialsAndIPv6(t *testing.T) {
	got := postgresURL("::1", 55432, "dev@flow", "p:a/ss", "app name")
	want := "postgres://dev%40flow:p%3Aa%2Fss@[::1]:55432/app%20name?sslmode=disable"
	if got != want {
		t.Fatalf("unexpected escaped database URL: got %q want %q", got, want)
	}
}

type response struct {
	out         []byte
	err         error
	waitContext bool
	sequence    []response
}

type fakeRunner struct {
	responses map[string]response
	calls     []string
	builds    []dockerBuildSpec
}

func (f *fakeRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := key(name, args...)
	f.calls = append(f.calls, call)
	if res, ok := f.responses[call]; ok {
		if len(res.sequence) > 0 {
			next := res.sequence[0]
			res.sequence = res.sequence[1:]
			f.responses[call] = res
			res = next
		}
		if res.waitContext {
			<-ctx.Done()
			return res.out, ctx.Err()
		}
		return res.out, res.err
	}
	return nil, nil
}

func (f *fakeRunner) sawPrefix(prefix string) bool {
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeRunner) calledBefore(firstPrefix, secondPrefix string) bool {
	first := -1
	second := -1
	for index, call := range f.calls {
		if first < 0 && strings.HasPrefix(call, firstPrefix) {
			first = index
		}
		if second < 0 && strings.HasPrefix(call, secondPrefix) {
			second = index
		}
	}
	return first >= 0 && second >= 0 && first < second
}

func (f *fakeRunner) callCount(want string) int {
	count := 0
	for _, call := range f.calls {
		if call == want {
			count++
		}
	}
	return count
}

func (f *fakeRunner) callWithPrefix(prefix string) string {
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return call
		}
	}
	return ""
}

func key(name string, args ...string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func portInspectKey(container string) string {
	return key("docker", "inspect", "-f", `{{range (index .NetworkSettings.Ports "5432/tcp")}}{{.HostPort}}{{"\n"}}{{end}}`, container)
}

func imageInspectKey(container string) string {
	return key("docker", "inspect", "-f", "{{.Config.Image}}", container)
}

func volumeInspectKey(container string) string {
	return key("docker", "inspect", "-f", `{{range .Mounts}}{{println .Type .Name .Destination}}{{end}}`, container)
}

func platformInspectKey(image string) string {
	return key("docker", "image", "inspect", "-f", "{{.Os}}/{{.Architecture}}", image)
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func jsonWrite(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
