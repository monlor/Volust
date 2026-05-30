package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	volustdocker "github.com/monlor/volust/internal/docker"
)

func TestRunDaemonOnceLoadsConfig(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return &appFakeRuntime{}, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	err := Run([]string{"daemon", "--config", path, "--once"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "loaded 1 profile") {
		t.Fatalf("output = %q", got)
	}
}

func TestRunDaemonOnceUsesEnvironmentConfigByDefault(t *testing.T) {
	t.Setenv("VOLUST_S3_REPOSITORY", "s3:s3.amazonaws.com/bucket/app")
	t.Setenv("RESTIC_PASSWORD", "secret")

	var out bytes.Buffer
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return &appFakeRuntime{}, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	err := Run([]string{"daemon", "--once"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "loaded 1 profile(s): default") {
		t.Fatalf("output = %q", got)
	}
}

func TestMaxConcurrentWritesDefaultsToFour(t *testing.T) {
	limiter, err := maxConcurrentWritesLimiter()
	if err != nil {
		t.Fatalf("maxConcurrentWritesLimiter returned error: %v", err)
	}
	if got := limiter.Capacity(); got != 4 {
		t.Fatalf("capacity = %d, want 4", got)
	}
}

func TestMaxConcurrentWritesReadsEnvironment(t *testing.T) {
	t.Setenv("VOLUST_MAX_CONCURRENT_WRITES", "2")
	limiter, err := maxConcurrentWritesLimiter()
	if err != nil {
		t.Fatalf("maxConcurrentWritesLimiter returned error: %v", err)
	}
	if got := limiter.Capacity(); got != 2 {
		t.Fatalf("capacity = %d, want 2", got)
	}
}

func TestMaxConcurrentWritesZeroDisablesLimit(t *testing.T) {
	t.Setenv("VOLUST_MAX_CONCURRENT_WRITES", "0")
	limiter, err := maxConcurrentWritesLimiter()
	if err != nil {
		t.Fatalf("maxConcurrentWritesLimiter returned error: %v", err)
	}
	if got := limiter.Capacity(); got != 0 {
		t.Fatalf("capacity = %d, want 0", got)
	}
}

func TestMaxConcurrentWritesRejectsInvalidEnvironment(t *testing.T) {
	t.Setenv("VOLUST_MAX_CONCURRENT_WRITES", "abc")
	if _, err := maxConcurrentWritesLimiter(); err == nil || !strings.Contains(err.Error(), "VOLUST_MAX_CONCURRENT_WRITES") {
		t.Fatalf("error = %v, want VOLUST_MAX_CONCURRENT_WRITES parse error", err)
	}
	t.Setenv("VOLUST_MAX_CONCURRENT_WRITES", "-1")
	if _, err := maxConcurrentWritesLimiter(); err == nil || !strings.Contains(err.Error(), "must be >= 0") {
		t.Fatalf("error = %v, want non-negative validation error", err)
	}
}

func TestRunRestoreRequiresConfirmationPhrase(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data", "--snapshot", "latest"}, strings.NewReader("no\n"), &out)
	if err == nil {
		t.Fatal("Run succeeded without confirmation phrase")
	}
	if !strings.Contains(out.String(), "RESTORE postgres/data") {
		t.Fatalf("restore prompt output = %q", out.String())
	}
}

func TestRunRestoreStartsWritableRestoreJobAfterConfirmation(t *testing.T) {
	path := writeConfig(t)
	t.Setenv("VOLUST_JOB_IMAGE", "ghcr.io/monlor/volust:test")
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data", "--snapshot", "latest", "--skip-pre-backup"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(fake.jobs) != 2 {
		t.Fatalf("jobs started = %d", len(fake.jobs))
	}
	job := fake.jobs[1]
	if job.Name != "volust-restore-postgres-data" {
		t.Fatalf("job name = %q", job.Name)
	}
	if job.Image != "ghcr.io/monlor/volust:test" {
		t.Fatalf("job image = %q", job.Image)
	}
	if job.Mounts[0].Target != "/volust/target" || job.Mounts[0].ReadOnly {
		t.Fatalf("restore mount = %#v", job.Mounts[0])
	}
	if script := job.Args[2]; !strings.Contains(script, "--tag app:postgres") || !strings.Contains(script, "--tag profile:s3prod") || !strings.Contains(script, "--tag source:data") {
		t.Fatalf("restore command does not filter latest snapshot by tags: %q", script)
	}
}

func TestRunRestoreStopsRunningContainerBacksUpRestoresAndRestoresRunningState(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":   "true",
				"volust.profile":   "s3prod",
				"volust.sources":   "/data",
				"volust.schedule":  "0 3 * * *",
				"volust.retention": "keep-last=1",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !equalStrings(fake.events, []string{"job:snapshots", "stop:abc", "job:backup", "job:restore", "start:abc"}) {
		t.Fatalf("events = %#v", fake.events)
	}
	if script := fake.jobs[len(fake.jobs)-1].Args[2]; !strings.Contains(script, "restore snap-before-pre-backup") {
		t.Fatalf("restore did not pin latest before pre-backup: %q", script)
	}
}

func TestRunRestoreValidatesExplicitSnapshotBeforeStoppingOrPreBackup(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
		snapshotOutput: `[]`,
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data", "--snapshot", "missing"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err == nil {
		t.Fatal("Run succeeded with missing explicit snapshot")
	}
	if !strings.Contains(err.Error(), "snapshot missing not found") {
		t.Fatalf("Run error = %v", err)
	}
	if !equalStrings(fake.events, []string{"job:snapshots"}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestRunRestoreCanSkipPreBackup(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data", "--skip-pre-backup"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !equalStrings(fake.events, []string{"job:snapshots", "stop:abc", "job:restore", "start:abc"}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestRunRestoreUsesIncludeStoppedEnvironment(t *testing.T) {
	t.Setenv("VOLUST_INCLUDE_STOPPED_CONTAINERS", "true")
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: false,
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data", "--skip-pre-backup"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !fake.sawIncludeStopped {
		t.Fatalf("list options = %#v", fake.lastListOptions)
	}
	if !equalStrings(fake.events, []string{"job:snapshots", "job:restore"}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestRunRestoreStopsOtherRunningContainersUsingSameSource(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{
			{
				ID:      "abc",
				Name:    "/postgres",
				Running: true,
				Labels: map[string]string{
					"volust.enabled":  "true",
					"volust.profile":  "s3prod",
					"volust.sources":  "/data",
					"volust.schedule": "0 3 * * *",
				},
				Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
			},
			{
				ID:      "sidecar",
				Name:    "/postgres-sidecar",
				Running: true,
				Mounts:  []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
			},
		},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data", "--skip-pre-backup"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	want := []string{"job:snapshots", "stop:abc", "stop:sidecar", "job:restore", "start:sidecar", "start:abc"}
	if !equalStrings(fake.events, want) {
		t.Fatalf("events = %#v, want %#v", fake.events, want)
	}
}

func TestRunRestoreRestartsStoppedContainersWhenRestoreJobFails(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
		runJobErrByOperation: map[string]error{"restore": errors.New("restore canceled")},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data", "--skip-pre-backup"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err == nil {
		t.Fatal("Run succeeded after restore job failure")
	}
	want := []string{"job:snapshots", "stop:abc", "job:restore", "start:abc"}
	if !equalStrings(fake.events, want) {
		t.Fatalf("events = %#v, want %#v", fake.events, want)
	}
}

func TestRunRestoreWithMaxConcurrentWritesOneDoesNotDeadlockPreBackup(t *testing.T) {
	t.Setenv("VOLUST_MAX_CONCURRENT_WRITES", "1")
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":   "true",
				"volust.profile":   "s3prod",
				"volust.sources":   "/data",
				"volust.schedule":  "0 3 * * *",
				"volust.retention": "keep-last=1",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data"}, strings.NewReader("RESTORE postgres/data\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !equalStrings(fake.events, []string{"job:snapshots", "stop:abc", "job:backup", "job:restore", "start:abc"}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestRestartContainersUsesCleanupContextAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &appFakeRuntime{}

	err := restartContainers(ctx, fake, []string{"abc"}, errors.New("restore canceled"))
	if err == nil {
		t.Fatal("restartContainers dropped the original error")
	}
	if fake.sawCanceledStartContext {
		t.Fatal("restartContainers used the canceled restore context for cleanup")
	}
	if !equalStrings(fake.events, []string{"start:abc"}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestRunRestorePromptsForMissingAppAndSource(t *testing.T) {
	t.Setenv("VOLUST_S3_REPOSITORY", "s3:s3.amazonaws.com/bucket/app")
	t.Setenv("RESTIC_PASSWORD", "secret")
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.sources":  "/data,/config",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{
				{Type: "volume", Name: "pgdata", Destination: "/data"},
				{Type: "volume", Name: "pgconfig", Destination: "/config"},
			},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	input := strings.NewReader("1\n1\nRESTORE postgres/config\n")
	err := Run([]string{"restore", "--skip-pre-backup"}, input, &out)
	if err != nil {
		t.Fatalf("Run returned error: %v\noutput: %s", err, out.String())
	}
	if len(fake.jobs) != 2 {
		t.Fatalf("jobs started = %d", len(fake.jobs))
	}
	if fake.jobs[1].Name != "volust-restore-postgres-config" {
		t.Fatalf("job name = %q", fake.jobs[1].Name)
	}
	if !strings.Contains(out.String(), "Select restore mode") || !strings.Contains(out.String(), "Select application") || !strings.Contains(out.String(), "Select source") {
		t.Fatalf("interactive output = %q", out.String())
	}
}

func TestRunRestoreAllVolumesInteractivelyRestoresNamedVolumes(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{
			{
				ID:      "pg",
				Name:    "/postgres",
				Running: true,
				Labels: map[string]string{
					"volust.enabled":  "true",
					"volust.profile":  "s3prod",
					"volust.sources":  "/data,/cache",
					"volust.schedule": "0 3 * * *",
				},
				Mounts: []volustdocker.Mount{
					{Type: "volume", Name: "pgdata", Destination: "/data"},
					{Type: "bind", Source: "/srv/postgres/cache", Destination: "/cache"},
				},
			},
			{
				ID:      "redis",
				Name:    "/redis",
				Running: true,
				Labels: map[string]string{
					"volust.enabled":  "true",
					"volust.profile":  "s3prod",
					"volust.sources":  "/data",
					"volust.schedule": "0 3 * * *",
				},
				Mounts: []volustdocker.Mount{{Type: "volume", Name: "redisdata", Destination: "/data"}},
			},
		},
		snapshotOutput: `[
			{"short_id":"pg-snap","id":"pg-snap","time":"2026-01-02T00:00:00Z","tags":["volust","app:postgres","profile:s3prod","source:data"]},
			{"short_id":"redis-snap","id":"redis-snap","time":"2026-01-02T00:00:00Z","tags":["volust","app:redis","profile:s3prod","source:data"]}
		]`,
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--skip-pre-backup"}, strings.NewReader("2\nRESTORE ALL VOLUMES\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v\noutput: %s", err, out.String())
	}
	want := []string{"job:snapshots", "job:snapshots", "stop:pg", "job:restore", "start:pg", "stop:redis", "job:restore", "start:redis"}
	if !equalStrings(fake.events, want) {
		t.Fatalf("events = %#v, want %#v", fake.events, want)
	}
	output := out.String()
	for _, want := range []string{"Select restore mode", "Volumes: 2", "postgres/data -> pgdata", "redis/data -> redisdata", "restore jobs completed: volumes=2"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q does not contain %q", output, want)
		}
	}
	for _, job := range fake.jobs {
		if strings.Contains(strings.Join(job.Args, " "), "/srv/postgres/cache") {
			t.Fatalf("bind source was restored: %#v", fake.jobs)
		}
	}
}

func TestRunRestoreAllVolumesFlagBypassesModePrompt(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "pg",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--all-volumes", "--skip-pre-backup"}, strings.NewReader("RESTORE ALL VOLUMES\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v\noutput: %s", err, out.String())
	}
	if strings.Contains(out.String(), "Select restore mode") {
		t.Fatalf("all-volumes flag should not prompt for mode: %q", out.String())
	}
	if !equalStrings(fake.events, []string{"job:snapshots", "job:restore"}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestRunRestoreAllVolumesConfirmationMismatchPreventsWork(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "pg",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--all-volumes"}, strings.NewReader("no\n"), &out)
	if err == nil {
		t.Fatal("Run succeeded without confirmation phrase")
	}
	if len(fake.events) != 0 {
		t.Fatalf("events = %#v, want no work before confirmation", fake.events)
	}
}

func TestRunRestoreAllVolumesPreflightFailurePreventsDestructiveWork(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "pg",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
		snapshotOutput: `[]`,
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"restore", "--config", path, "--profile", "s3prod", "--all-volumes"}, strings.NewReader("RESTORE ALL VOLUMES\n"), &out)
	if err == nil {
		t.Fatal("Run succeeded without a matching snapshot")
	}
	if !strings.Contains(err.Error(), "no matching snapshot found") {
		t.Fatalf("error = %v", err)
	}
	if !equalStrings(fake.events, []string{"job:snapshots"}) {
		t.Fatalf("events = %#v, want only snapshot preflight", fake.events)
	}
}

func TestRunAppsListsDiscoveredApplications(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":   "true",
				"volust.profile":   "s3prod",
				"volust.sources":   "/data,/config",
				"volust.schedule":  "0 3 * * *",
				"volust.retention": "keep-last=7",
			},
			Mounts: []volustdocker.Mount{
				{Type: "volume", Name: "pgdata", Destination: "/data"},
				{Type: "volume", Name: "pgconfig", Destination: "/config"},
			},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"apps", "--config", path, "--profile", "s3prod"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	output := out.String()
	for _, want := range []string{"postgres", "data", "config", "0 3 * * *", "running"} {
		if !strings.Contains(output, want) {
			t.Fatalf("apps output %q does not contain %q", output, want)
		}
	}
}

func TestRunSnapshotsPromptsForAppAndSource(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data,/config",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{
				{Type: "volume", Name: "pgdata", Destination: "/data"},
				{Type: "volume", Name: "pgconfig", Destination: "/config"},
			},
		}},
		snapshotOutput: `[{"short_id":"snap-new","id":"snap-new","time":"2026-01-02T00:00:00Z","tags":["volust","app:postgres","profile:s3prod","source:config"]},{"short_id":"snap-old","id":"snap-old","time":"2026-01-01T00:00:00Z","tags":["volust","app:postgres","profile:s3prod","source:config"]}]`,
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"snapshots", "--config", path, "--profile", "s3prod"}, strings.NewReader("1\n2\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v\noutput: %s", err, out.String())
	}
	if !equalStrings(fake.events, []string{"job:snapshots"}) {
		t.Fatalf("events = %#v", fake.events)
	}
	output := out.String()
	for _, want := range []string{"Select application", "Select source", "snap-new", "snap-old", "2026-01-02T00:00:00Z"} {
		if !strings.Contains(output, want) {
			t.Fatalf("snapshots output %q does not contain %q", output, want)
		}
	}
	if got := strings.Join(fake.jobs[0].Args, " "); !strings.Contains(got, "source:config") || strings.Contains(got, " latest ") {
		t.Fatalf("snapshots command = %q", got)
	}
}

func TestRunSnapshotsUsesParameters(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
		snapshotOutput: `[{"short_id":"snap-1","id":"snap-1","time":"2026-01-01T00:00:00Z","tags":["volust","app:postgres","profile:s3prod","source:data"]}]`,
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"snapshots", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if strings.Contains(out.String(), "Select application") || !strings.Contains(out.String(), "snap-1") {
		t.Fatalf("snapshots output = %q", out.String())
	}
}

func TestRunSnapshotsReportsInvalidSnapshotOutput(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
		snapshotOutput: "rclone backend error: directory not found\n",
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"snapshots", "--config", path, "--profile", "s3prod", "--app", "postgres", "--source", "data"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("Run succeeded with invalid snapshot output")
	}
	got := err.Error()
	for _, want := range []string{"snapshots output is not valid JSON", "invalid character 'r'", "rclone backend error: directory not found"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error %q does not contain %q", got, want)
		}
	}
}

func TestRunBackupUsesAppParameterAndBacksUpAllSources(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":   "true",
				"volust.profile":   "s3prod",
				"volust.sources":   "/data,/config",
				"volust.schedule":  "0 3 * * *",
				"volust.retention": "keep-last=7",
			},
			Mounts: []volustdocker.Mount{
				{Type: "volume", Name: "pgdata", Destination: "/data"},
				{Type: "volume", Name: "pgconfig", Destination: "/config"},
			},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"backup", "--config", path, "--profile", "s3prod", "--app", "postgres"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !equalStrings(fake.events, []string{"job:backup", "job:forget", "job:backup", "job:forget", "job:prune"}) {
		t.Fatalf("events = %#v", fake.events)
	}
	if !strings.Contains(out.String(), "backup complete: app=postgres sources=2 jobs_started=5") {
		t.Fatalf("backup output = %q", out.String())
	}
}

func TestRunBackupUsesStopBeforeBackupEnvironment(t *testing.T) {
	t.Setenv("VOLUST_STOP_CONTAINERS_BEFORE_BACKUP", "true")
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":  "true",
				"volust.profile":  "s3prod",
				"volust.sources":  "/data",
				"volust.schedule": "0 3 * * *",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"backup", "--config", path, "--profile", "s3prod", "--app", "postgres"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v\noutput: %s", err, out.String())
	}
	if !equalStrings(fake.events, []string{"stop:abc", "job:backup", "start:abc", "job:prune"}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestRunBackupPromptsForMissingAppAndSupportsSourceParameter(t *testing.T) {
	path := writeConfig(t)
	fake := &appFakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":   "true",
				"volust.profile":   "s3prod",
				"volust.sources":   "/data,/config",
				"volust.schedule":  "0 3 * * *",
				"volust.retention": "keep-last=7",
			},
			Mounts: []volustdocker.Mount{
				{Type: "volume", Name: "pgdata", Destination: "/data"},
				{Type: "volume", Name: "pgconfig", Destination: "/config"},
			},
		}},
	}
	oldRuntime := newRuntime
	newRuntime = func() (daemonRuntime, error) {
		return fake, nil
	}
	defer func() {
		newRuntime = oldRuntime
	}()

	var out bytes.Buffer
	err := Run([]string{"backup", "--config", path, "--profile", "s3prod", "--source", "config"}, strings.NewReader("1\n"), &out)
	if err != nil {
		t.Fatalf("Run returned error: %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "Select application") {
		t.Fatalf("interactive output = %q", out.String())
	}
	if !equalStrings(fake.events, []string{"job:backup", "job:forget", "job:prune"}) {
		t.Fatalf("events = %#v", fake.events)
	}
	if len(fake.jobs) == 0 || !strings.Contains(fake.jobs[0].Args[2], "/volust/sources/config") {
		t.Fatalf("backup job = %#v", fake.jobs)
	}
}

func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := []byte("profiles:\n  s3prod:\n    type: s3\n    repository: s3:s3.amazonaws.com/bucket/app\n    password: test\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type appFakeRuntime struct {
	containers              []volustdocker.Container
	jobs                    []volustdocker.JobSpec
	events                  []string
	lastListOptions         volustdocker.ListOptions
	sawIncludeStopped       bool
	sawCanceledStartContext bool
	runJobErrByOperation    map[string]error
	snapshotOutput          string
}

func (f *appFakeRuntime) ListContainers(_ context.Context, options volustdocker.ListOptions) ([]volustdocker.Container, error) {
	f.lastListOptions = options
	if options.IncludeStopped {
		f.sawIncludeStopped = true
	}
	return f.containers, nil
}

func (f *appFakeRuntime) RunJob(_ context.Context, job volustdocker.JobSpec) error {
	f.jobs = append(f.jobs, job)
	f.events = append(f.events, "job:"+job.Operation)
	if f.runJobErrByOperation != nil {
		return f.runJobErrByOperation[job.Operation]
	}
	return nil
}

func (f *appFakeRuntime) RunJobOutput(_ context.Context, job volustdocker.JobSpec) ([]byte, error) {
	f.jobs = append(f.jobs, job)
	f.events = append(f.events, "job:"+job.Operation)
	if f.snapshotOutput != "" {
		return []byte(f.snapshotOutput), nil
	}
	source := "data"
	profile := "s3prod"
	if len(job.Args) > 0 {
		joined := strings.Join(job.Args, " ")
		if strings.Contains(joined, "source:config") {
			source = "config"
		}
		if strings.Contains(joined, "profile:default") {
			profile = "default"
		}
	}
	return []byte(`[{"short_id":"snap-before-pre-backup","id":"snap-before-pre-backup","time":"2026-01-01T00:00:00Z","tags":["volust","app:postgres","profile:` + profile + `","source:` + source + `"]}]`), nil
}

func (f *appFakeRuntime) StopContainer(_ context.Context, id string) error {
	f.events = append(f.events, "stop:"+id)
	return nil
}

func (f *appFakeRuntime) StartContainer(ctx context.Context, id string) error {
	if ctx.Err() != nil {
		f.sawCanceledStartContext = true
	}
	f.events = append(f.events, "start:"+id)
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
