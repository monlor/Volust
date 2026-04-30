package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	report, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest"})
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if report.Discovered != 1 || report.JobsStarted != 3 {
		t.Fatalf("report = %#v", report)
	}
	if got := runtime.jobs[0].Name; got != "volust-backup-postgres-data" {
		t.Fatalf("first job name = %q", got)
	}
	if got := runtime.jobs[1].Operation; got != "forget" {
		t.Fatalf("second job command = %#v", runtime.jobs[1].Args)
	}
	if got := runtime.jobs[2].Operation; got != "prune" {
		t.Fatalf("third job command = %#v", runtime.jobs[2].Args)
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	report, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest"})
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

	report, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest"})
	if err != nil {
		t.Fatalf("RunOnce returned error: %v", err)
	}
	if report.Discovered != 1 || report.JobsStarted != 3 {
		t.Fatalf("report = %#v", report)
	}
	if got := runtime.jobs[0].Name; got != "volust-backup-postgres-data" {
		t.Fatalf("first job name = %q", got)
	}
}

func TestRunOncePassesIncludeStoppedOptionToRuntime(t *testing.T) {
	runtime := &fakeRuntime{}
	cfg := config.Config{Profiles: map[string]config.Profile{
		"default": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest", IncludeStopped: true}); err != nil {
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest"}); err != nil {
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer
	spec, err := volustdocker.ParseBackupSpec(runtime.containers[0], cfg.Profiles)
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	profile := cfg.Profiles[spec.Profile]
	runScheduledSourceJobs(context.Background(), runtime, Options{JobImage: "volust:latest", LogWriter: &log}, profile, spec, spec.Sources[0])
	if !strings.Contains(log.String(), "restic failed") {
		t.Fatalf("scheduler did not log job failure, log=%q", log.String())
	}
	if got := log.String(); !strings.Contains(got, "postgres") || !strings.Contains(got, "data") {
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest", ExcludeDir: dir}); err != nil {
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"default": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer

	report, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest", LogWriter: &log})
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer

	if _, err := RunOnce(context.Background(), cfg, runtime, Options{JobImage: "volust:latest", LogWriter: &log}); err != nil {
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
	cfg := config.Config{Profiles: map[string]config.Profile{
		"s3prod": {Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"},
	}}
	var log bytes.Buffer

	report, err := RunScheduler(ctx, cfg, runtime, Options{JobImage: "volust:latest", RefreshInterval: time.Millisecond, LogWriter: &log})
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
	spec, err := volustdocker.ParseBackupSpec(volustdocker.Container{
		ID:   "abc",
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.profile":  "s3prod",
			"volust.sources":  "/data",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []volustdocker.Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
	}, map[string]config.Profile{"s3prod": {Type: config.ProfileS3}})
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RunSourceJobs(context.Background(), runtime, Options{JobImage: "volust:latest"}, profile, spec, spec.Sources[0], false); err != nil {
				t.Errorf("RunSourceJobs returned error: %v", err)
			}
		}()
	}
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("jobs overlapped before release, maxRunning=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("jobs overlapped, maxRunning=%d", got)
	}
}

func TestRunSourceJobsSerializesSamePhysicalVolumeAcrossApps(t *testing.T) {
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
		if _, err := RunSourceJobs(context.Background(), runtime, Options{JobImage: "volust:latest"}, profile, specA, specA.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs A returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := RunSourceJobs(context.Background(), runtime, Options{JobImage: "volust:latest"}, profile, specB, specB.Sources[0], false); err != nil {
			t.Errorf("RunSourceJobs B returned error: %v", err)
		}
	}()
	for atomic.LoadInt32(&runtime.started) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("jobs overlapped before release, maxRunning=%d", got)
	}
	close(runtime.release)
	wg.Wait()
	if got := atomic.LoadInt32(&runtime.maxRunning); got != 1 {
		t.Fatalf("jobs overlapped, maxRunning=%d", got)
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

func (f *lockingRuntime) RunJob(_ context.Context, _ volustdocker.JobSpec) error {
	running := atomic.AddInt32(&f.running, 1)
	for {
		maxRunning := atomic.LoadInt32(&f.maxRunning)
		if running <= maxRunning || atomic.CompareAndSwapInt32(&f.maxRunning, maxRunning, running) {
			break
		}
	}
	if atomic.AddInt32(&f.started, 1) == 1 {
		<-f.release
	}
	atomic.AddInt32(&f.running, -1)
	return nil
}

type fakeRuntime struct {
	containers      []volustdocker.Container
	containerSets   [][]volustdocker.Container
	jobs            []volustdocker.JobSpec
	runErr          error
	listCalls       int
	onList          func(int)
	lastListOptions volustdocker.ListOptions
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

func (f *fakeRuntime) RunJob(_ context.Context, job volustdocker.JobSpec) error {
	f.jobs = append(f.jobs, job)
	return f.runErr
}
