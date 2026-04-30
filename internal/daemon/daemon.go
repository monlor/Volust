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
	RunJob(context.Context, volustdocker.JobSpec) error
	StopContainer(context.Context, string) error
	StartContainer(context.Context, string) error
}

type Options struct {
	JobImage         string
	ConfigDir        string
	ExcludeDir       string
	LogWriter        io.Writer
	RefreshInterval  time.Duration
	IncludeStopped   bool
	StopBeforeBackup bool
	AssumeLocked     bool
	SkipRetention    bool
}

type Report struct {
	Discovered  int
	Skipped     int
	Scheduled   int
	JobsStarted int
}

func RunOnce(ctx context.Context, cfg config.Config, runtime Runtime, options Options) (Report, error) {
	if options.JobImage == "" {
		options.JobImage = "volust:latest"
	}
	if options.ExcludeDir == "" {
		options.ExcludeDir = "/etc/volust/excludes"
	}

	containers, err := runtime.ListContainers(ctx, volustdocker.ListOptions{IncludeStopped: options.IncludeStopped})
	if err != nil {
		return Report{}, err
	}

	var report Report
	pruneProfiles := map[string]config.Profile{}
	pruneNames := map[string]string{}
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
		profile := cfg.Profiles[spec.Profile]
		pruneProfiles[spec.Profile] = profile
		if pruneNames[spec.Profile] == "" {
			pruneNames[spec.Profile] = spec.Name
		}
		for _, source := range spec.Sources {
			logDiscovered(options.LogWriter, spec, source)
			jobsStarted, err := RunSourceJobs(ctx, runtime, options, profile, spec, source, false)
			if err != nil {
				return report, err
			}
			report.JobsStarted += jobsStarted
		}
	}
	profileNames := make([]string, 0, len(pruneProfiles))
	for profileName := range pruneProfiles {
		profileNames = append(profileNames, profileName)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		profile := pruneProfiles[profileName]
		if err := runPruneJob(ctx, runtime, options, profile, pruneNames[profileName]); err != nil {
			return report, err
		}
		report.JobsStarted++
	}
	return report, nil
}

func RunScheduler(ctx context.Context, cfg config.Config, runtime Runtime, options Options) (Report, error) {
	if options.JobImage == "" {
		options.JobImage = "volust:latest"
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
		profile := cfg.Profiles[spec.Profile]
		for _, source := range spec.Sources {
			key := scheduleKey(spec, source)
			current[key] = struct{}{}
			if _, ok := entries[key]; ok {
				continue
			}
			logDiscovered(options.LogWriter, spec, source)
			spec := spec
			source := source
			profile := profile
			entryID, err := scheduler.AddFunc(spec.Schedule.Expr, func() {
				runScheduledSourceJobs(ctx, runtime, options, profile, spec, source)
			})
			if err != nil {
				return report, err
			}
			entries[key] = entryID
		}
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

func scheduleKey(spec volustdocker.BackupSpec, source volustdocker.Source) string {
	return strings.Join([]string{spec.ContainerID, spec.Profile, spec.Name, source.ID, spec.Schedule.Expr}, "\x00")
}

func runScheduledSourceJobs(ctx context.Context, runtime Runtime, options Options, profile config.Profile, spec volustdocker.BackupSpec, source volustdocker.Source) {
	if _, err := RunSourceJobs(ctx, runtime, options, profile, spec, source, true); err != nil && options.LogWriter != nil {
		fmt.Fprintf(options.LogWriter, "scheduled backup failed app=%s profile=%s source=%s: %v\n", spec.Name, spec.Profile, source.ID, err)
	}
}

func RunSourceJobs(ctx context.Context, runtime Runtime, options Options, profile config.Profile, spec volustdocker.BackupSpec, source volustdocker.Source, includePrune bool) (int, error) {
	if !options.AssumeLocked {
		var jobsStarted int
		err := WithSourceLock(ctx, SourceLockKey(spec.Profile, spec, source), func() error {
			var err error
			jobsStarted, err = runSourceJobsLocked(ctx, runtime, options, profile, spec, source, includePrune)
			return err
		})
		return jobsStarted, err
	}
	return runSourceJobsLocked(ctx, runtime, options, profile, spec, source, includePrune)
}

func runSourceJobsLocked(ctx context.Context, runtime Runtime, options Options, profile config.Profile, spec volustdocker.BackupSpec, source volustdocker.Source, includePrune bool) (int, error) {
	if options.JobImage == "" {
		options.JobImage = "volust:latest"
	}
	if options.ExcludeDir == "" {
		options.ExcludeDir = "/etc/volust/excludes"
	}
	excludeFiles := make([]string, 0, len(spec.ExcludeFiles))
	for _, file := range spec.ExcludeFiles {
		excludeFiles = append(excludeFiles, filepath.Join(options.ExcludeDir, file))
	}

	commands := []restic.Command{
		restic.BackupCommand(profile, spec, source, excludeFiles),
	}
	if !options.SkipRetention && len(spec.Retention.Args()) > 0 {
		commands = append(commands, restic.ForgetCommand(profile, spec, source))
	}
	if includePrune {
		commands = append(commands, restic.PruneCommand(profile))
	}

	jobsStarted := 0
	for _, command := range commands {
		jobName := fmt.Sprintf("%s-%s", spec.Name, source.ID)
		job := volustdocker.BuildBackupJob(volustdocker.JobRequest{
			Name:   jobName,
			Image:  options.JobImage,
			Source: source,
			Args:   command.Args,
			Env:    command.Env,
		})
		job.Operation = command.Operation
		if command.Operation != "backup" {
			job.Name = fmt.Sprintf("volust-%s-%s", command.Operation, jobName)
		}
		if command.Operation == "backup" && shouldStopBeforeBackup(options, spec) {
			if err := runJobWithStoppedContainer(ctx, runtime, spec.ContainerID, job); err != nil {
				return jobsStarted, err
			}
		} else {
			if err := runtime.RunJob(ctx, job); err != nil {
				return jobsStarted, err
			}
		}
		jobsStarted++
	}
	return jobsStarted, nil
}

func shouldStopBeforeBackup(options Options, spec volustdocker.BackupSpec) bool {
	stop := options.StopBeforeBackup
	if spec.StopBeforeBackupSet {
		stop = spec.StopBeforeBackup
	}
	return stop && spec.ContainerRunning && spec.ContainerID != ""
}

func runJobWithStoppedContainer(ctx context.Context, runtime Runtime, containerID string, job volustdocker.JobSpec) error {
	return WithSourceLock(ctx, ContainerLockKey(containerID), func() error {
		if err := runtime.StopContainer(ctx, containerID); err != nil {
			return err
		}
		jobErr := runtime.RunJob(ctx, job)
		return restartBackupContainer(containerID, runtime, jobErr)
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
	command := restic.PruneCommand(profile)
	job := volustdocker.JobSpec{
		Name:      "volust-prune-" + strings.ReplaceAll(name, "/", "-"),
		Image:     options.JobImage,
		Operation: command.Operation,
		Args:      command.Args,
		Env:       command.Env,
	}
	return runtime.RunJob(ctx, job)
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
