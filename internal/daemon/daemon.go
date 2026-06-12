package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/monlor/volust/internal/config"
	volustdocker "github.com/monlor/volust/internal/docker"
	"github.com/monlor/volust/internal/restic"
)

type Runtime interface {
	ListContainers(context.Context, volustdocker.ListOptions) ([]volustdocker.Container, error)
	RunWorker(context.Context, volustdocker.WorkerSpec) error
	StopContainer(context.Context, string) error
	StartContainer(context.Context, string) error
}

type Options struct {
	WorkerImage      string
	ConfigDir        string
	ExcludeDir       string
	LogWriter        io.Writer
	RefreshInterval  time.Duration
	IncludeStopped   bool
	StopBeforeBackup bool
	WriteLimiter     *WriteLimiter
	AssumeLocked     bool
	AssumeWriteSlot  bool
	SkipRetention    bool
}

type Report struct {
	Discovered  int
	Skipped     int
	Scheduled   int
	JobsStarted int
}

func RunOnce(ctx context.Context, cfg config.Config, runtime Runtime, options Options) (Report, error) {
	if options.WorkerImage == "" {
		options.WorkerImage = "volust:latest"
	}
	if options.ExcludeDir == "" {
		options.ExcludeDir = "/etc/volust/excludes"
	}

	containers, err := runtime.ListContainers(ctx, volustdocker.ListOptions{IncludeStopped: options.IncludeStopped})
	if err != nil {
		return Report{}, err
	}

	var report Report
	for _, container := range containers {
		spec, err := volustdocker.ParseBackupSpecWithDefaults(container, cfg.Profiles, cfg.Defaults)
		if err != nil {
			report.Skipped++
			logSkip(options.LogWriter, container, err)
			continue
		}
		report.Discovered++
		if err := LoadExcludeFiles(options.ExcludeDir, &spec); err != nil {
			return report, err
		}
		for _, source := range spec.Sources {
			logDiscovered(options.LogWriter, spec, source)
		}
		profile := cfg.Profiles[spec.Profile]
		jobsStarted, err := RunSpecJobs(ctx, runtime, options, profile, spec, spec.Sources, true)
		if err != nil {
			return report, err
		}
		report.JobsStarted += jobsStarted
	}
	return report, nil
}

func RunScheduler(ctx context.Context, cfg config.Config, runtime Runtime, options Options) (Report, error) {
	if options.WorkerImage == "" {
		options.WorkerImage = "volust:latest"
	}
	if options.ExcludeDir == "" {
		options.ExcludeDir = "/etc/volust/excludes"
	}

	containers, err := runtime.ListContainers(ctx, volustdocker.ListOptions{IncludeStopped: options.IncludeStopped})
	if err != nil {
		return Report{}, err
	}

	scheduler := cron.New()
	defer scheduler.Stop()
	if options.RefreshInterval == 0 {
		options.RefreshInterval = time.Minute
	}
	entries := map[string]cron.EntryID{}
	var report Report
	report, err = reconcileSchedules(ctx, scheduler, entries, cfg, containers, runtime, options)
	if err != nil {
		return report, err
	}
	scheduler.Start()

	ticker := time.NewTicker(options.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-ticker.C:
			containers, err := runtime.ListContainers(ctx, volustdocker.ListOptions{IncludeStopped: options.IncludeStopped})
			if err != nil {
				return report, err
			}
			report, err = reconcileSchedules(ctx, scheduler, entries, cfg, containers, runtime, options)
			if err != nil {
				return report, err
			}
		}
	}
}

func reconcileSchedules(ctx context.Context, scheduler *cron.Cron, entries map[string]cron.EntryID, cfg config.Config, containers []volustdocker.Container, runtime Runtime, options Options) (Report, error) {
	current := map[string]struct{}{}
	var report Report
	for _, container := range containers {
		spec, err := volustdocker.ParseBackupSpecWithDefaults(container, cfg.Profiles, cfg.Defaults)
		if err != nil {
			report.Skipped++
			logSkip(options.LogWriter, container, err)
			continue
		}
		if err := LoadExcludeFiles(options.ExcludeDir, &spec); err != nil {
			return report, err
		}
		report.Discovered++
		key := scheduleKey(spec)
		current[key] = struct{}{}
		if _, ok := entries[key]; ok {
			continue
		}
		for _, source := range spec.Sources {
			logDiscovered(options.LogWriter, spec, source)
		}
		scheduledSpec := spec
		profile := cfg.Profiles[scheduledSpec.Profile]
		entryID, err := scheduler.AddFunc(scheduledSpec.Schedule.Expr, func() {
			runScheduledSpecJobs(ctx, runtime, options, profile, scheduledSpec)
		})
		if err != nil {
			return report, err
		}
		entries[key] = entryID
	}
	for key, entryID := range entries {
		if _, ok := current[key]; !ok {
			scheduler.Remove(entryID)
			delete(entries, key)
		}
	}
	report.Scheduled = len(entries)
	return report, nil
}

func scheduleKey(spec volustdocker.BackupSpec) string {
	sourceIDs := make([]string, 0, len(spec.Sources))
	for _, source := range spec.Sources {
		sourceIDs = append(sourceIDs, source.ID)
	}
	sort.Strings(sourceIDs)
	return strings.Join([]string{spec.ContainerID, spec.Profile, spec.Name, spec.Schedule.Expr, strings.Join(sourceIDs, ",")}, "\x00")
}

func runScheduledSpecJobs(ctx context.Context, runtime Runtime, options Options, profile config.Profile, spec volustdocker.BackupSpec) {
	if _, err := RunSpecJobs(ctx, runtime, options, profile, spec, spec.Sources, true); err != nil && options.LogWriter != nil {
		fmt.Fprintf(options.LogWriter, "scheduled backup failed app=%s profile=%s: %v\n", spec.Name, spec.Profile, err)
	}
}

func RunSourceJobs(ctx context.Context, runtime Runtime, options Options, profile config.Profile, spec volustdocker.BackupSpec, source volustdocker.Source, includePrune bool) (int, error) {
	return RunSpecJobs(ctx, runtime, options, profile, spec, []volustdocker.Source{source}, includePrune)
}

func RunSpecJobs(ctx context.Context, runtime Runtime, options Options, profile config.Profile, spec volustdocker.BackupSpec, sources []volustdocker.Source, includePrune bool) (int, error) {
	appProfile := profile.ForApp(spec.Name)
	if !options.AssumeLocked {
		var jobsStarted int
		err := WithSourceLock(ctx, RepositoryLockKey(appProfile), func() error {
			var err error
			jobsStarted, err = runSpecJobsLocked(ctx, runtime, options, appProfile, spec, sources, includePrune)
			return err
		})
		return jobsStarted, err
	}
	return runSpecJobsLocked(ctx, runtime, options, appProfile, spec, sources, includePrune)
}

func runSpecJobsLocked(ctx context.Context, runtime Runtime, options Options, profile config.Profile, spec volustdocker.BackupSpec, sources []volustdocker.Source, includePrune bool) (int, error) {
	if options.WorkerImage == "" {
		options.WorkerImage = "volust:latest"
	}
	if options.ExcludeDir == "" {
		options.ExcludeDir = "/etc/volust/excludes"
	}
	excludeFiles := make([]string, 0, len(spec.ExcludeFiles))
	for _, file := range spec.ExcludeFiles {
		excludeFiles = append(excludeFiles, filepath.Join(options.ExcludeDir, file))
	}

	var commands []restic.Command
	var mounts []volustdocker.JobMount
	for _, source := range sources {
		mounts = append(mounts, volustdocker.BuildSourceMount(source, restic.SourcePath(spec, source), true))
		commands = append(commands, restic.BackupCommand(profile, spec, source, excludeFiles))
		if !options.SkipRetention && len(spec.Retention.Args()) > 0 {
			commands = append(commands, restic.ForgetCommand(profile, spec, source))
		}
	}
	if includePrune {
		commands = append(commands, restic.PruneCommand(profile))
	}
	workerCommands := make([]volustdocker.WorkerCommand, 0, len(commands))
	for _, command := range commands {
		workerCommands = append(workerCommands, volustdocker.WorkerCommand{
			Operation: command.Operation,
			Args:      command.Args,
			Env:       command.Env,
		})
	}
	worker := volustdocker.WorkerSpec{
		Name:     volustdocker.WorkerName("backup", spec.Name),
		Image:    options.WorkerImage,
		Env:      profile.ResticEnv(),
		Mounts:   mounts,
		Commands: workerCommands,
	}
	err := withWriteSlot(ctx, options, profile, func() error {
		if shouldStopBeforeBackup(options, spec) {
			return runWorkerWithStoppedContainer(ctx, runtime, spec.ContainerID, worker)
		}
		return runtime.RunWorker(ctx, worker)
	})
	if err != nil {
		return 0, err
	}
	return len(workerCommands), nil
}

func shouldStopBeforeBackup(options Options, spec volustdocker.BackupSpec) bool {
	stop := options.StopBeforeBackup
	if spec.StopBeforeBackupSet {
		stop = spec.StopBeforeBackup
	}
	return stop && spec.ContainerRunning && spec.ContainerID != ""
}

func runWorkerWithStoppedContainer(ctx context.Context, runtime Runtime, containerID string, worker volustdocker.WorkerSpec) error {
	return WithSourceLock(ctx, ContainerLockKey(containerID), func() error {
		if err := runtime.StopContainer(ctx, containerID); err != nil {
			return err
		}
		workerErr := runtime.RunWorker(ctx, worker)
		return restartBackupContainer(containerID, runtime, workerErr)
	})
}

func restartBackupContainer(containerID string, runtime Runtime, err error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	startErr := runtime.StartContainer(cleanupCtx, containerID)
	if err != nil && startErr != nil {
		return errors.Join(err, startErr)
	}
	if err != nil {
		return err
	}
	return startErr
}

func runPruneJob(ctx context.Context, runtime Runtime, options Options, profile config.Profile, name string) error {
	if options.WorkerImage == "" {
		options.WorkerImage = "volust:latest"
	}
	appProfile := profile.ForApp(name)
	command := restic.PruneCommand(appProfile)
	worker := volustdocker.WorkerSpec{
		Name:  volustdocker.WorkerName("prune", name),
		Image: options.WorkerImage,
		Env:   appProfile.ResticEnv(),
		Commands: []volustdocker.WorkerCommand{{
			Operation: command.Operation,
			Args:      command.Args,
			Env:       command.Env,
		}},
	}
	return WithSourceLock(ctx, RepositoryLockKey(appProfile), func() error {
		return withWriteSlot(ctx, options, appProfile, func() error {
			return runtime.RunWorker(ctx, worker)
		})
	})
}

func RunPruneJob(ctx context.Context, runtime Runtime, options Options, profile config.Profile, name string) error {
	return runPruneJob(ctx, runtime, options, profile, name)
}

func withWriteSlot(ctx context.Context, options Options, profile config.Profile, fn func() error) error {
	if options.AssumeWriteSlot || options.WriteLimiter == nil {
		return fn()
	}
	return options.WriteLimiter.With(ctx, BackendWriteKey(profile), fn)
}

func LoadExcludeFiles(excludeDir string, spec *volustdocker.BackupSpec) error {
	for _, file := range spec.ExcludeFiles {
		patterns, err := readExcludePatterns(filepath.Join(excludeDir, file))
		if err != nil {
			return err
		}
		spec.Excludes = append(spec.Excludes, patterns...)
	}
	spec.ExcludeFiles = nil
	return nil
}

func readExcludePatterns(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, scanner.Err()
}

func logSkip(writer io.Writer, container volustdocker.Container, err error) {
	if writer == nil {
		return
	}
	name := strings.TrimPrefix(container.Name, "/")
	if name == "" {
		name = container.ID
	}
	fmt.Fprintf(writer, "skipping container=%s: %v\n", name, err)
}

func logDiscovered(writer io.Writer, spec volustdocker.BackupSpec, source volustdocker.Source) {
	if writer == nil {
		return
	}
	fmt.Fprintf(writer, "info backup enabled app discovered app=%s profile=%s source=%s schedule=%q\n", spec.Name, spec.Profile, source.ID, spec.Schedule.Expr)
}
