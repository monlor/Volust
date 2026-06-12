package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monlor/volust/internal/config"
	volustdocker "github.com/monlor/volust/internal/docker"
)

func TestRunOnceCreatesBackupForgetAndPruneJobsPerSource(t *testing.T) {
	runtime := &fakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":   "true",
				"volust.profile":   "s3prod",
				"volust.sources":   "/data",
				"volust.schedule":  "0 3 * * *",
				"volust.retention": "keep-last=7",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	report, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest"})
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if report.Discovered != 1 || report.JobsStarted != 3 {
		t.Fatalf("report = %#v", report)
	}
	if got := runtime.jobs[0].Name; got != "volust-backup-postgres" {
		t.Fatalf("first job name = %q", got)
	}
	if got := runtime.jobs[1].Operation; got != "forget" {
		t.Fatalf("second job command = %#v", runtime.jobs[1].Args)
	}
	if got := runtime.jobs[2].Operation; got != "prune" {
		t.Fatalf("third job command = %#v", runtime.jobs[2].Args)
	}
	if len(runtime.events) != 3 || runtime.events[0] != "job:backup" || runtime.events[1] != "job:forget" || runtime.events[2] != "job:prune" {
		t.Fatalf("events = %#v", runtime.events)
	}
}

func TestRunOnceStopsRunningContainerWhenGlobalStopBeforeBackupEnabled(t *testing.T) {
	runtime := &fakeRuntime{
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
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest", StopBeforeBackup: true}); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	want := []string{"stop:abc", "job:backup", "job:prune", "start:abc"}
	if !equalStrings(runtime.events, want) {
		t.Fatalf("events = %#v, want %#v", runtime.events, want)
	}
}

func TestRunOnceStopBeforeBackupLabelOverridesGlobalDefault(t *testing.T) {
	runtime := &fakeRuntime{
		containers: []volustdocker.Container{{
			ID:      "abc",
			Name:    "/postgres",
			Running: true,
			Labels: map[string]string{
				"volust.enabled":            "true",
				"volust.profile":            "s3prod",
				"volust.sources":            "/data",
				"volust.schedule":           "0 3 * * *",
				"volust.stop-before-backup": "false",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest", StopBeforeBackup: true}); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	want := []string{"job:backup", "job:prune"}
	if !equalStrings(runtime.events, want) {
		t.Fatalf("events = %#v, want %#v", runtime.events, want)
	}
}

func TestRunOnceSkipsForgetWhenRetentionIsUnset(t *testing.T) {
	runtime := &fakeRuntime{
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
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	report, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest"})
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if report.Discovered != 1 || report.JobsStarted != 2 {
		t.Fatalf("report = %#v", report)
	}
	if len(runtime.jobs) != 2 {
		t.Fatalf("jobs started = %d", len(runtime.jobs))
	}
	if got := runtime.jobs[0].Args[2]; !strings.Contains(got, " backup ") {
		t.Fatalf("first job command = %#v", runtime.jobs[0].Args)
	}
	if got := runtime.jobs[1].Operation; got != "prune" {
		t.Fatalf("second job command = %#v", runtime.jobs[1].Args)
	}
}

func TestRunOnceUsesDefaultPolicyAndSources(t *testing.T) {
	runtime := &fakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled": "true",
				"volust.profile": "s3prod",
			},
			Mounts: []volustdocker.Mount{
				{Type: "volume", Name: "pgdata", Destination: "/data"},
				{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
			},
		}},
	}
	cfg := config.Config{
		Defaults: config.PolicyDefaults{Schedule: "0 3 * * *", Retention: "keep-last=7"},
		Profiles: map[string]config.Profile{
			"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
		},
	}

	report, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest"})
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if report.Discovered != 1 || report.JobsStarted != 3 {
		t.Fatalf("report = %#v", report)
	}
	if got := runtime.jobs[0].Name; got != "volust-backup-postgres" {
		t.Fatalf("first job name = %q", got)
	}
}

func TestRunOncePassesIncludeStoppedOptionToRuntime(t *testing.T) {
	runtime := &fakeRuntime{}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"default": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest", IncludeStopped: true}); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if !runtime.lastListOptions.IncludeStopped {
		t.Fatalf("list options = %#v", runtime.lastListOptions)
	}
}

func TestRunOncePrunesOncePerProfile(t *testing.T) {
	runtime := &fakeRuntime{
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
	}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest"}); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	prunes := 0
	for _, job := range runtime.jobs {
		if job.Operation == "prune" {
			prunes++
		}
	}
	if prunes != 1 {
		t.Fatalf("prune jobs = %d, jobs = %#v", prunes, runtime.jobs)
	}
	if got := runtime.jobs[len(runtime.jobs)-1].Operation; got != "prune" {
		t.Fatalf("last job operation = %q, jobs = %#v", got, runtime.jobs)
	}
}

func TestRunPruneJobSerializesSameAppRepository(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := runPruneJob(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, "postgres"); err != nil {
			t.Errorf("runPruneJob A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := runPruneJob(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, "postgres"); err != nil {
			t.Errorf("runPruneJob B returned error: %v", err)
		}
	}()
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("prune jobs overlapped before release, maxRunning=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("prune jobs overlapped, maxRunning=%d", got)
	}
}

func TestRunPruneJobAllowsDifferentAppsToRunConcurrently(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := runPruneJob(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, "postgres"); err != nil {
			t.Errorf("runPruneJob A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := runPruneJob(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, "redis"); err != nil {
			t.Errorf("runPruneJob B returned error: %v", err)
		}
	}()
	deadline := time.After(time.Second)
	for atomic.LoadInt32(&runtime.started) < 2 {
		select {
		case <-deadline:
			close(runtime.release)
			wg.Wait()
			t.Fatalf("different app prune jobs did not start concurrently, started=%d", atomic.LoadInt32(&runtime.started))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 2 {
		t.Fatalf("different app prune jobs did not run concurrently, maxRunning=%d", got)
	}
}

func TestRunSchedulerLogsSourceJobFailures(t *testing.T) {
	runtime := &fakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":   "true",
				"volust.profile":   "s3prod",
				"volust.sources":   "/data",
				"volust.schedule":  "* * * * *",
				"volust.retention": "keep-last=7",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
		runErr: errors.New("restic failed"),
	}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer
	spec, err := volustdocker.ParseBackupSpecWithDefaults(runtime.containers[0], cfg.Profiles, config.PolicyDefaults{Schedule: "0 3 * * *"})
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	profile := cfg.Profiles[spec.Profile]
	runScheduledSpecJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", LogWriter: &log}, profile, spec)
	if !strings.Contains(log.String(), "restic failed") {
		t.Fatalf("scheduler did not log job failure, log=%q", log.String())
	}
	if got := log.String(); !strings.Contains(got, "postgres") {
		t.Fatalf("scheduler log lacks job context: %q", got)
	}
}

func TestRunOnceLoadsExcludeFilesIntoBackupCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "media.txt"), []byte("cache/**\n\n# comment\ntmp/**\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{
		containers: []volustdocker.Container{{
			ID:   "abc",
			Name: "/postgres",
			Labels: map[string]string{
				"volust.enabled":      "true",
				"volust.profile":      "s3prod",
				"volust.sources":      "/data",
				"volust.schedule":     "0 3 * * *",
				"volust.exclude-file": "media.txt",
			},
			Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
		}},
	}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest", ExcludeDir: dir}); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	script := runtime.jobs[0].Args[2]
	if strings.Contains(script, "--exclude-file") {
		t.Fatalf("backup command still references exclude file path: %q", script)
	}
	for _, want := range []string{"--exclude 'cache/**'", "--exclude 'tmp/**'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("backup command %q does not contain %q", script, want)
		}
	}
}

func TestRunOnceLogsSkippedContainerReasons(t *testing.T) {
	runtime := &fakeRuntime{
		containers: []volustdocker.Container{{
			ID:     "abc",
			Name:   "/postgres",
			Labels: map[string]string{"volust.enabled": "true"},
		}},
	}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"default": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer

	report, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest", LogWriter: &log})
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if report.Skipped != 1 {
		t.Fatalf("report = %#v", report)
	}
	if got := log.String(); !strings.Contains(got, "skipping container") || !strings.Contains(got, "postgres") || !strings.Contains(got, "no backup sources found") {
		t.Fatalf("skip log = %q", got)
	}
}

func TestRunOnceLogsDiscoveredBackupApplications(t *testing.T) {
	runtime := &fakeRuntime{
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
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{WorkerImage: "volust:latest", LogWriter: &log}); err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	got := log.String()
	for _, want := range []string{"backup enabled app discovered", "app=postgres", "profile=s3prod", "source=data", "schedule=\"0 3 * * *\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("discovery log %q does not contain %q", got, want)
		}
	}
}

func TestRunSchedulerRescansForNewContainers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &fakeRuntime{
		containerSets: [][]volustdocker.Container{
			nil,
			{{
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
		},
		onList: func(call int) {
			if call == 2 {
				cancel()
			}
		},
	}
	cfg := config.Config{Defaults: config.PolicyDefaults{Schedule: "0 3 * * *"}, Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer

	report, err := RunScheduler(ctx, cfg, runtime, Options{WorkerImage: "volust:latest", RefreshInterval: time.Millisecond, LogWriter: &log})
	if err != context.Canceled {
		t.Fatalf("RunScheduler error = %v", err)
	}
	if report.Scheduled != 1 {
		t.Fatalf("report = %#v", report)
	}
	got := log.String()
	for _, want := range []string{"backup enabled app discovered", "app=postgres", "profile=s3prod", "source=data", "schedule=\"0 3 * * *\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("scheduler discovery log %q does not contain %q", got, want)
		}
	}
}

func TestRunSourceJobsSerializesSameSource(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	spec, err := volustdocker.ParseBackupSpecWithDefaults(volustdocker.Container{
		ID:   "abc",
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.profile":  "s3prod",
			"volust.sources":  "/data",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
	}, map[string]config.Profile{"s3prod": {Type: config.ProfileS3}}, config.PolicyDefaults{Schedule: "0 3 * * *"})
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, spec, spec.Sources[0], false); err != nil {
				t.Errorf("RunSourceJobs returned error: %v", err)
			}
		}()
	}
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		close(runtime.release)
		wg.Wait()
		t.Fatalf("jobs overlapped before release, maxRunning=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("jobs overlapped, maxRunning=%d", got)
	}
}

func TestRunSourceJobsAllowsDifferentAppsOnSharedVolumeToRunConcurrently(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}
	specA := volustdocker.BackupSpec{
		Name:    "postgres",
		Profile: "s3prod",
		Sources: []volustdocker.Source{{
			ID:         "data",
			Type:       "volume",
			VolumeName: "shared-data",
		}},
	}
	specB := volustdocker.BackupSpec{
		Name:    "postgres-sidecar",
		Profile: "s3prod",
		Sources: []volustdocker.Source{{
			ID:         "backup",
			Type:       "volume",
			VolumeName: "shared-data",
		}},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, specA, specA.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, specB, specB.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs B returned error: %v", err)
		}
	}()
	deadline := time.After(time.Second)
	for atomic.LoadInt32(&runtime.started) < 2 {
		select {
		case <-deadline:
			close(runtime.release)
			wg.Wait()
			t.Fatalf("different app jobs on shared volume did not start concurrently, started=%d", atomic.LoadInt32(&runtime.started))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 2 {
		t.Fatalf("different app jobs on shared volume did not run concurrently, maxRunning=%d", got)
	}
}

func TestRunSourceJobsSerializesSameAppRepositoryAcrossSources(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}
	specA := volustdocker.BackupSpec{
		Name:    "postgres",
		Profile: "s3prod",
		Sources: []volustdocker.Source{{
			ID:         "data",
			Type:       "volume",
			VolumeName: "pgdata",
		}},
	}
	specB := volustdocker.BackupSpec{
		Name:    "postgres",
		Profile: "s3prod",
		Sources: []volustdocker.Source{{
			ID:         "data",
			Type:       "volume",
			VolumeName: "redisdata",
		}},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, specA, specA.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profile, specB, specB.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs B returned error: %v", err)
		}
	}()
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		close(runtime.release)
		wg.Wait()
		t.Fatalf("jobs overlapped before release, maxRunning=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("jobs overlapped, maxRunning=%d", got)
	}
}

func TestRunSourceJobsAllowsDifferentAppsInSameProfileToRunConcurrently(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	profileA := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}
	profileB := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}
	specA := volustdocker.BackupSpec{
		Name:    "postgres",
		Profile: "s3prod-a",
		Sources: []volustdocker.Source{{
			ID:         "data",
			Type:       "volume",
			VolumeName: "pgdata",
		}},
	}
	specB := volustdocker.BackupSpec{
		Name:    "redis",
		Profile: "s3prod-b",
		Sources: []volustdocker.Source{{
			ID:         "data",
			Type:       "volume",
			VolumeName: "redisdata",
		}},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profileA, specA, specA.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest"}, profileB, specB, specB.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs B returned error: %v", err)
		}
	}()
	deadline := time.After(time.Second)
	for atomic.LoadInt32(&runtime.started) < 2 {
		select {
		case <-deadline:
			close(runtime.release)
			wg.Wait()
			t.Fatalf("different app jobs did not start concurrently, started=%d", atomic.LoadInt32(&runtime.started))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 2 {
		t.Fatalf("different app jobs did not run concurrently, maxRunning=%d", got)
	}
}

func TestRunSourceJobsLimitsWritesPerBackend(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	limiter := NewWriteLimiter(2)
	profiles := []config.Profile{
		{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app-a", Password: "secret"},
		{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app-b", Password: "secret"},
		{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app-c", Password: "secret"},
	}
	specs := []volustdocker.BackupSpec{
		{Name: "postgres", Profile: "s3prod-a", Sources: []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "pgdata"}}},
		{Name: "redis", Profile: "s3prod-b", Sources: []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "redisdata"}}},
		{Name: "memos", Profile: "s3prod-c", Sources: []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "memosdata"}}},
	}

	var wg sync.WaitGroup
	wg.Add(len(specs))
	for i := range specs {
		i := i
		go func() {
			defer wg.Done()
			if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", WriteLimiter: limiter}, profiles[i], specs[i], specs[i].Sources[0], false); err != nil {
				t.Errorf("RunSourceJobs %d returned error: %v", i, err)
			}
		}()
	}
	deadline := time.After(time.Second)
	for atomic.LoadInt32(&runtime.started) < 2 {
		select {
		case <-deadline:
			close(runtime.release)
			wg.Wait()
			t.Fatalf("two jobs did not start under backend limit, started=%d maxRunning=%d", atomic.LoadInt32(&runtime.started), atomic.LoadInt32(&runtime.maxRunning))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 2 {
		close(runtime.release)
		wg.Wait()
		t.Fatalf("backend limit did not allow exactly two jobs before release, maxRunning=%d", got)
	}
	if got := atomic.LoadInt32(&runtime.started); got != 2 {
		close(runtime.release)
		wg.Wait()
		t.Fatalf("third job started before a slot was released, started=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 2 {
		t.Fatalf("jobs exceeded backend limit, maxRunning=%d", got)
	}
}

func TestRunSourceJobsAllowsDifferentBackendsToRunConcurrently(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	limiter := NewWriteLimiter(1)
	profileA := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket-a/volust", Password: "secret"}
	profileB := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket-b/volust", Password: "secret"}
	specA := volustdocker.BackupSpec{Name: "postgres", Profile: "s3prod-a", Sources: []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "pgdata"}}}
	specB := volustdocker.BackupSpec{Name: "redis", Profile: "s3prod-b", Sources: []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "redisdata"}}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", WriteLimiter: limiter}, profileA, specA, specA.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", WriteLimiter: limiter}, profileB, specB, specB.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs B returned error: %v", err)
		}
	}()
	deadline := time.After(time.Second)
	for atomic.LoadInt32(&runtime.started) < 2 {
		select {
		case <-deadline:
			close(runtime.release)
			wg.Wait()
			t.Fatalf("different backend jobs did not start concurrently, started=%d maxRunning=%d", atomic.LoadInt32(&runtime.started), atomic.LoadInt32(&runtime.maxRunning))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 2 {
		t.Fatalf("different backend jobs did not overlap, maxRunning=%d", got)
	}
}

func TestWriteLimiterCoordinatesAcrossInstances(t *testing.T) {
	lockDir := t.TempDir()
	first := NewWriteLimiterWithDir(1, lockDir)
	second := NewWriteLimiterWithDir(1, lockDir)
	release := make(chan struct{})
	started := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.WithDefault(context.Background(), func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := second.WithDefault(ctx, func() error {
		t.Fatal("second limiter instance acquired the only slot while first held it")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-firstDone
		t.Fatalf("second With error = %v, want context deadline exceeded", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first With returned error: %v", err)
	}
	if err := second.WithDefault(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("second With after release returned error: %v", err)
	}
}

func TestWriteLimiterUsesLockTimeoutForWaiting(t *testing.T) {
	t.Setenv("VOLUST_LOCK_TIMEOUT", "25ms")
	lockDir := t.TempDir()
	first := NewWriteLimiterWithDir(1, lockDir)
	second := NewWriteLimiterWithDir(1, lockDir)
	release := make(chan struct{})
	started := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.With(context.Background(), "backend", func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	err := second.With(context.Background(), "backend", func() error {
		t.Fatal("second limiter instance acquired the only backend slot")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-firstDone
		t.Fatalf("second With error = %v, want context deadline exceeded", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first With returned error: %v", err)
	}
}

func TestWriteLimiterCoordinatesAcrossProcesses(t *testing.T) {
	lockDir := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	release := filepath.Join(t.TempDir(), "release")

	first := exec.Command(os.Args[0], "-test.run=TestWriteLimiterProcessHelper")
	first.Env = append(os.Environ(),
		"VOLUST_WRITE_LIMITER_HELPER=hold",
		"VOLUST_WRITE_LIMITER_DIR="+lockDir,
		"VOLUST_WRITE_LIMITER_READY="+ready,
		"VOLUST_WRITE_LIMITER_RELEASE="+release,
	)
	if err := first.Start(); err != nil {
		t.Fatalf("start first helper: %v", err)
	}
	defer first.Process.Kill()

	waitForFile(t, ready)
	second := exec.Command(os.Args[0], "-test.run=TestWriteLimiterProcessHelper")
	second.Env = append(os.Environ(),
		"VOLUST_WRITE_LIMITER_HELPER=timeout",
		"VOLUST_WRITE_LIMITER_DIR="+lockDir,
	)
	output, err := second.CombinedOutput()
	if err != nil {
		_ = os.WriteFile(release, []byte("release"), 0o600)
		_ = first.Wait()
		t.Fatalf("second helper failed: %v\n%s", err, output)
	}

	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatalf("write release file: %v", err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first helper failed: %v", err)
	}
}

func TestWriteLimiterProcessHelper(t *testing.T) {
	mode := os.Getenv("VOLUST_WRITE_LIMITER_HELPER")
	if mode == "" {
		return
	}
	dir := os.Getenv("VOLUST_WRITE_LIMITER_DIR")
	limiter := NewWriteLimiterWithDir(1, dir)
	switch mode {
	case "hold":
		err := limiter.WithDefault(context.Background(), func() error {
			if err := os.WriteFile(os.Getenv("VOLUST_WRITE_LIMITER_READY"), []byte("ready"), 0o600); err != nil {
				return err
			}
			waitForFile(t, os.Getenv("VOLUST_WRITE_LIMITER_RELEASE"))
			return nil
		})
		if err != nil {
			t.Fatalf("hold helper With returned error: %v", err)
		}
	case "timeout":
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := limiter.WithDefault(ctx, func() error {
			t.Fatal("timeout helper acquired the only slot while another process held it")
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout helper With error = %v, want context deadline exceeded", err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunSourceJobsStillSerializesSameAppRepositoryBeforeBackendWriteLimit(t *testing.T) {
	runtime := &lockingRuntime{release: make(chan struct{})}
	limiter := NewWriteLimiter(4)
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}
	specA := volustdocker.BackupSpec{Name: "postgres", Profile: "s3prod", Sources: []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "pgdata"}}}
	specB := volustdocker.BackupSpec{Name: "postgres", Profile: "s3prod", Sources: []volustdocker.Source{{ID: "config", Type: "volume", VolumeName: "pgconfig"}}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", WriteLimiter: limiter}, profile, specA, specA.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", WriteLimiter: limiter}, profile, specB, specB.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs B returned error: %v", err)
		}
	}()
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		close(runtime.release)
		wg.Wait()
		t.Fatalf("same app repository jobs overlapped before release, maxRunning=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("same app repository jobs overlapped, maxRunning=%d", got)
	}
}

func TestRepositoryLockKeyUsesAppRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := []byte(`
profiles:
  dav-a:
    type: webdav
    path: backups
    password: secret
    webdav:
      url: https://dav.example.com/remote.php/dav/files/alice
  dav-b:
    type: webdav
    path: backups
    password: secret
    webdav:
      url: https://dav.example.com/remote.php/dav/files/alice
  dav-c:
    type: webdav
    path: other
    password: secret
    webdav:
      url: https://dav.example.com/remote.php/dav/files/alice
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	keyA := RepositoryLockKey(cfg.Profiles["dav-a"])
	keyB := RepositoryLockKey(cfg.Profiles["dav-b"])
	keyC := RepositoryLockKey(cfg.Profiles["dav-c"])
	if keyA != keyB {
		t.Fatalf("same WebDAV node repository should share repository lock: %q != %q", keyA, keyB)
	}
	if keyA == keyC {
		t.Fatalf("different WebDAV node repositories should not share repository lock: %q", keyA)
	}
	if keyA != RepositoryLockKey(cfg.Profiles["dav-a"]) {
		t.Fatalf("same node repository should have a stable repository lock: %q", keyA)
	}
}

func TestBackendWriteKeyIgnoresAppRepositoryPath(t *testing.T) {
	profileA := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/volust/postgres"}
	profileB := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/other/redis"}
	profileC := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/other/volust/postgres"}
	if BackendWriteKey(profileA) != BackendWriteKey(profileB) {
		t.Fatalf("same S3 endpoint and bucket should share backend key: %q != %q", BackendWriteKey(profileA), BackendWriteKey(profileB))
	}
	if BackendWriteKey(profileA) == BackendWriteKey(profileC) {
		t.Fatalf("different S3 bucket should not share backend key: %q", BackendWriteKey(profileA))
	}
}

func TestRunSourceJobsSerializesStopBeforeBackupByContainer(t *testing.T) {
	runtime := &containerStopRuntime{release: make(chan struct{})}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}
	spec := volustdocker.BackupSpec{
		ContainerID:         "abc",
		ContainerRunning:    true,
		Name:                "postgres",
		Profile:             "s3prod",
		StopBeforeBackup:    true,
		StopBeforeBackupSet: true,
		Sources: []volustdocker.Source{
			{ID: "data", Type: "volume", VolumeName: "pgdata"},
			{ID: "config", Type: "volume", VolumeName: "pgconfig"},
		},
	}

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for _, source := range spec.Sources {
		source := source
		go func() {
			defer wg.Done()
			_, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", StopBeforeBackup: true}, profile, spec, source, false)
			errs <- err
		}()
	}
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	close(runtime.release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("RunSourceJobs returned error: %v", err)
		}
	}
	if got := atomic.LoadInt32(&runtime.stopAttempts); got != 2 {
		t.Fatalf("stop attempts = %d, want 2", got)
	}
}

func TestRunSourceJobsAcquiresGlobalWriteSlotBeforeStoppingContainer(t *testing.T) {
	runtime := &containerStopRuntime{release: make(chan struct{})}
	limiter := NewWriteLimiter(1)
	profileA := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app-a", Password: "secret"}
	profileB := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app-b", Password: "secret"}
	specA := volustdocker.BackupSpec{Name: "postgres", Profile: "s3prod-a", Sources: []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "pgdata"}}}
	specB := volustdocker.BackupSpec{
		ContainerID:      "abc",
		ContainerRunning: true,
		Name:             "redis",
		Profile:          "s3prod-b",
		Sources:          []volustdocker.Source{{ID: "data", Type: "volume", VolumeName: "redisdata"}},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", WriteLimiter: limiter}, profileA, specA, specA.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs A returned error: %v", err)
		}
	}()
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{WorkerImage: "volust:latest", WriteLimiter: limiter, StopBeforeBackup: true}, profileB, specB, specB.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs B returned error: %v", err)
		}
	}()
	if got := atomic.LoadInt32(&runtime.stopAttempts); got != 0 {
		close(runtime.release)
		wg.Wait()
		t.Fatalf("container stopped before a global write slot was available, stopAttempts=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.stopAttempts); got != 1 {
		t.Fatalf("container was not stopped after slot became available, stopAttempts=%d", got)
	}
}

type lockingRuntime struct {
	started    int32
	running    int32
	maxRunning int32
	release    chan struct{}
}

func (f *lockingRuntime) ListContainers(_ context.Context, _ volustdocker.ListOptions) ([]volustdocker.Container, error) {
	return nil, nil
}

func (f *lockingRuntime) RunWorker(_ context.Context, _ volustdocker.WorkerSpec) error {
	running := atomic.AddInt32(&f.running, 1)
	for {
		maxRunning := atomic.LoadInt32(&f.maxRunning)
		if running <= maxRunning || atomic.CompareAndSwapInt32(&f.maxRunning, maxRunning, running) {
			break
		}
	}
	atomic.AddInt32(&f.started, 1)
	if f.release != nil {
		<-f.release
	}
	atomic.AddInt32(&f.running, -1)
	return nil
}

func (f *lockingRuntime) StopContainer(context.Context, string) error {
	return nil
}

func (f *lockingRuntime) StartContainer(context.Context, string) error {
	return nil
}

type containerStopRuntime struct {
	started      int32
	stopped      int32
	stopAttempts int32
	release      chan struct{}
}

func (f *containerStopRuntime) ListContainers(_ context.Context, _ volustdocker.ListOptions) ([]volustdocker.Container, error) {
	return nil, nil
}

func (f *containerStopRuntime) RunWorker(_ context.Context, _ volustdocker.WorkerSpec) error {
	atomic.AddInt32(&f.started, 1)
	if f.release != nil {
		<-f.release
	}
	return nil
}

func (f *containerStopRuntime) StopContainer(context.Context, string) error {
	atomic.AddInt32(&f.stopAttempts, 1)
	if !atomic.CompareAndSwapInt32(&f.stopped, 0, 1) {
		return errors.New("container already stopped")
	}
	return nil
}

func (f *containerStopRuntime) StartContainer(context.Context, string) error {
	if !atomic.CompareAndSwapInt32(&f.stopped, 1, 0) {
		return errors.New("container was not stopped")
	}
	return nil
}

type fakeRuntime struct {
	containers      []volustdocker.Container
	containerSets   [][]volustdocker.Container
	jobs            []fakeJob
	events          []string
	runErr          error
	listCalls       int
	onList          func(int)
	lastListOptions volustdocker.ListOptions
}

type fakeJob struct {
	Name      string
	Operation string
	Args      []string
	Mounts    []volustdocker.JobMount
}

func (f *fakeRuntime) ListContainers(_ context.Context, options volustdocker.ListOptions) ([]volustdocker.Container, error) {
	f.listCalls++
	f.lastListOptions = options
	if f.onList != nil {
		f.onList(f.listCalls)
	}
	if len(f.containerSets) > 0 {
		index := f.listCalls - 1
		if index >= len(f.containerSets) {
			index = len(f.containerSets) - 1
		}
		return f.containerSets[index], nil
	}
	return f.containers, nil
}

func (f *fakeRuntime) RunWorker(_ context.Context, worker volustdocker.WorkerSpec) error {
	for _, command := range worker.Commands {
		f.jobs = append(f.jobs, fakeJob{Name: worker.Name, Operation: command.Operation, Args: command.Args, Mounts: worker.Mounts})
		f.events = append(f.events, "job:"+command.Operation)
		if f.runErr != nil {
			return f.runErr
		}
	}
	return nil
}

func (f *fakeRuntime) StopContainer(_ context.Context, id string) error {
	f.events = append(f.events, "stop:"+id)
	return nil
}

func (f *fakeRuntime) StartContainer(_ context.Context, id string) error {
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
