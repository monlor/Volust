package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/monlor/volust/internal/config"
	"github.com/monlor/volust/internal/daemon"
	volustdocker "github.com/monlor/volust/internal/docker"
	"github.com/monlor/volust/internal/restic"
)

const DefaultWorkerImage = "ghcr.io/monlor/volust:latest"

type daemonRuntime interface {
	daemon.Runtime
	StopContainer(context.Context, string) error
	StartContainer(context.Context, string) error
	RunWorkerOutput(context.Context, volustdocker.WorkerSpec) ([]byte, error)
}

var newRuntime = func() (daemonRuntime, error) {
	return volustdocker.NewRuntime()
}

func Run(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return usage(out)
	}
	switch args[0] {
	case "daemon":
		return runDaemon(args[1:], out)
	case "apps":
		return runApps(args[1:], out)
	case "snapshots":
		return runSnapshots(args[1:], in, out)
	case "backup":
		return runBackup(args[1:], in, out)
	case "restore":
		return runRestore(args[1:], in, out)
	default:
		return usage(out)
	}
}

func runDaemon(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "optional path to config.yaml")
	once := fs.Bool("once", false, "load config and scan once")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadDefault(*configPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "loaded %d profile(s): %s\n", len(cfg.Profiles), strings.Join(profileNames(cfg), ","))
	if *once {
		runtime, err := newRuntime()
		if err != nil {
			return err
		}
		limiter, err := maxConcurrentWritesLimiter()
		if err != nil {
			return err
		}
		report, err := daemon.RunOnce(context.Background(), cfg, runtime, daemon.Options{WorkerImage: workerImage(), IncludeStopped: includeStoppedContainers(), StopBeforeBackup: stopContainersBeforeBackup(), WriteLimiter: limiter})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "scan complete: discovered=%d skipped=%d jobs_started=%d\n", report.Discovered, report.Skipped, report.JobsStarted)
		return nil
	}
	runtime, err := newRuntime()
	if err != nil {
		return err
	}
	limiter, err := maxConcurrentWritesLimiter()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(out, "daemon mode: scheduling discovered label-backed jobs")
	report, err := daemon.RunScheduler(ctx, cfg, runtime, daemon.Options{WorkerImage: workerImage(), LogWriter: out, IncludeStopped: includeStoppedContainers(), StopBeforeBackup: stopContainersBeforeBackup(), WriteLimiter: limiter})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Fprintf(out, "scheduler stopped: discovered=%d skipped=%d scheduled=%d\n", report.Discovered, report.Skipped, report.Scheduled)
	return nil
}

func runApps(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("apps", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "optional path to config.yaml")
	profile := fs.String("profile", "", "profile name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, profileName, err := loadConfigAndProfile(*configPath, *profile)
	if err != nil {
		return err
	}
	runtime, err := newRuntime()
	if err != nil {
		return err
	}
	candidates, err := restoreCandidates(context.Background(), runtime, cfg, profileName)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return fmt.Errorf("no applications found for profile=%s", profileName)
	}
	fmt.Fprintf(out, "Applications for profile=%s\n", profileName)
	for _, candidate := range candidates {
		state := "stopped"
		if candidate.Running {
			state = "running"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", candidate.Spec.Name, candidate.Source.ID, state, candidate.Spec.Schedule.Expr)
	}
	return nil
}

func runSnapshots(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "optional path to config.yaml")
	profile := fs.String("profile", "", "profile name")
	appName := fs.String("app", "", "application name")
	source := fs.String("source", "", "source id")
	snapshot := fs.String("snapshot", "", "optional snapshot id, for example latest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, profileName, err := loadConfigAndProfile(*configPath, *profile)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	runtime, err := newRuntime()
	if err != nil {
		return err
	}
	selected, err := resolveRestoreSelection(context.Background(), runtime, cfg, profileName, *appName, *source, reader, out)
	if err != nil {
		return err
	}
	snapshots, err := querySnapshots(context.Background(), runtime, cfg.Profiles[profileName], restic.RestoreRequest{
		SnapshotID: *snapshot,
		App:        selected.Spec.Name,
		Container:  selected.Spec.ContainerName,
		Profile:    profileName,
		SourceID:   selected.Source.ID,
	})
	if err != nil {
		return err
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Time > snapshots[j].Time
	})
	fmt.Fprintf(out, "Snapshots for profile=%s app=%s source=%s\n", profileName, selected.Spec.Name, selected.Source.ID)
	if len(snapshots) == 0 {
		fmt.Fprintln(out, "No snapshots found")
		return nil
	}
	for _, snapshot := range snapshots {
		fmt.Fprintf(out, "%s\t%s\n", snapshot.SnapshotID(), snapshot.Time)
	}
	return nil
}

func runBackup(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "optional path to config.yaml")
	profile := fs.String("profile", "", "profile name")
	appName := fs.String("app", "", "application name")
	source := fs.String("source", "", "optional source id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, profileName, err := loadConfigAndProfile(*configPath, *profile)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	runtime, err := newRuntime()
	if err != nil {
		return err
	}
	selected, err := resolveBackupSelection(context.Background(), runtime, cfg, profileName, *appName, *source, reader, out)
	if err != nil {
		return err
	}
	profileCfg := cfg.Profiles[profileName]
	limiter, err := maxConcurrentWritesLimiter()
	if err != nil {
		return err
	}
	spec := selected[0].Spec
	if err := daemon.LoadExcludeFiles("/etc/volust/excludes", &spec); err != nil {
		return err
	}
	sources := make([]volustdocker.Source, 0, len(selected))
	for _, candidate := range selected {
		sources = append(sources, candidate.Source)
	}
	jobsStarted, err := daemon.RunSpecJobs(context.Background(), runtime, daemon.Options{WorkerImage: workerImage(), StopBeforeBackup: stopContainersBeforeBackup(), WriteLimiter: limiter}, profileCfg, spec, sources, true)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "backup complete: app=%s sources=%d jobs_started=%d\n", selected[0].Spec.Name, len(selected), jobsStarted)
	return nil
}

func runRestore(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "optional path to config.yaml")
	profile := fs.String("profile", "", "profile name")
	appName := fs.String("app", "", "application name")
	source := fs.String("source", "", "source id")
	snapshot := fs.String("snapshot", "latest", "snapshot id")
	skipPreBackup := fs.Bool("skip-pre-backup", false, "skip safety backup before restore")
	allVolumes := fs.Bool("all-volumes", false, "restore all discovered Docker named volume sources")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, profileName, err := loadConfigAndProfile(*configPath, *profile)
	if err != nil {
		return err
	}
	*profile = profileName
	reader := bufio.NewReader(in)
	runtime, err := newRuntime()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !*allVolumes && *appName == "" && *source == "" {
		mode, err := promptChoice(reader, out, "Select restore mode", []string{"Restore one source", "Restore all volumes"})
		if err != nil {
			return err
		}
		*allVolumes = mode == "Restore all volumes"
	}
	if *allVolumes {
		return runRestoreAllVolumes(ctx, runtime, cfg, *profile, *snapshot, *skipPreBackup, reader, out)
	}

	selected, err := resolveRestoreSelection(ctx, runtime, cfg, *profile, *appName, *source, reader, out)
	if err != nil {
		return err
	}
	*appName = selected.Spec.Name
	*source = selected.Source.ID

	phrase := "RESTORE " + *appName + "/" + *source
	fmt.Fprintf(out, "Profile: %s\nApplication: %s\nSource: %s\nSnapshot: %s\n", *profile, *appName, *source, *snapshot)
	if err := confirmRestore(reader, out, phrase); err != nil {
		return err
	}

	profileCfg := cfg.Profiles[*profile]
	limiter, err := maxConcurrentWritesLimiter()
	if err != nil {
		return err
	}
	resolvedSnapshot, err := resolveSnapshot(ctx, runtime, profileCfg, restic.RestoreRequest{
		SnapshotID: *snapshot,
		App:        *appName,
		Container:  selected.Spec.ContainerName,
		Profile:    *profile,
		SourceID:   *source,
	})
	if err != nil {
		return err
	}
	err = restoreCandidateWithSnapshot(ctx, runtime, cfg, *profile, selected, resolvedSnapshot, *skipPreBackup, limiter)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "restore job completed")
	return nil
}

func runRestoreAllVolumes(ctx context.Context, runtime daemonRuntime, cfg config.Config, profileName, snapshotID string, skipPreBackup bool, reader *bufio.Reader, out io.Writer) error {
	selected, err := resolveAllVolumeRestoreSelection(ctx, runtime, cfg, profileName)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Profile: %s\nSnapshot: %s\nVolumes: %d\n", profileName, snapshotID, len(selected))
	for _, candidate := range selected {
		fmt.Fprintf(out, "  %s/%s -> %s\n", candidate.Spec.Name, candidate.Source.ID, candidate.Source.VolumeName)
	}
	if err := confirmRestore(reader, out, "RESTORE ALL VOLUMES"); err != nil {
		return err
	}

	profileCfg := cfg.Profiles[profileName]
	resolved, err := resolveRestoreSnapshots(ctx, runtime, profileCfg, profileName, snapshotID, selected)
	if err != nil {
		return err
	}
	limiter, err := maxConcurrentWritesLimiter()
	if err != nil {
		return err
	}
	for _, candidate := range selected {
		key := restoreSnapshotKey(candidate)
		if err := restoreCandidateWithSnapshot(ctx, runtime, cfg, profileName, candidate, resolved[key], skipPreBackup, limiter); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "restore jobs completed: volumes=%d\n", len(selected))
	return nil
}

func confirmRestore(reader *bufio.Reader, out io.Writer, phrase string) error {
	fmt.Fprintf(out, "This restore is destructive. Type %q to continue: ", phrase)
	answer, _ := reader.ReadString('\n')
	if strings.TrimSpace(answer) != phrase {
		return errors.New("restore confirmation phrase did not match")
	}
	return nil
}

func resolveRestoreSnapshots(ctx context.Context, runtime daemonRuntime, profile config.Profile, profileName, snapshotID string, selected []restoreCandidate) (map[string]string, error) {
	resolved := make(map[string]string, len(selected))
	for _, candidate := range selected {
		snapshot, err := resolveSnapshot(ctx, runtime, profile, restic.RestoreRequest{
			SnapshotID: snapshotID,
			App:        candidate.Spec.Name,
			Container:  candidate.Spec.ContainerName,
			Profile:    profileName,
			SourceID:   candidate.Source.ID,
		})
		if err != nil {
			return nil, err
		}
		resolved[restoreSnapshotKey(candidate)] = snapshot
	}
	return resolved, nil
}

func restoreSnapshotKey(candidate restoreCandidate) string {
	return candidate.Spec.Name + "\x00" + candidate.Source.ID
}

func restoreCandidateWithSnapshot(ctx context.Context, runtime daemonRuntime, cfg config.Config, profileName string, selected restoreCandidate, snapshotID string, skipPreBackup bool, limiter *daemon.WriteLimiter) error {
	profileCfg := cfg.Profiles[profileName]
	return daemon.WithSourceLock(ctx, daemon.RepositoryLockKey(profileCfg), func() error {
		return daemon.WithSourceLock(ctx, daemon.SourceLockKey(profileName, selected.Spec, selected.Source), func() error {
			return limiter.With(ctx, daemon.BackendWriteKey(profileCfg), func() error {
				stopped, err := stopMountedContainers(ctx, runtime, selected)
				if err != nil {
					return err
				}
				return restoreWithStoppedContainers(ctx, runtime, cfg, profileName, selected.Spec.Name, selected.Source.ID, snapshotID, skipPreBackup, selected, stopped)
			})
		})
	})
}

func loadConfigAndProfile(configPath, profileName string) (config.Config, string, error) {
	cfg, err := config.LoadDefault(configPath)
	if err != nil {
		return config.Config{}, "", err
	}
	if profileName == "" {
		profileName = "default"
	}
	if _, ok := cfg.Profiles[profileName]; !ok {
		return config.Config{}, "", fmt.Errorf("unknown profile %q", profileName)
	}
	return cfg, profileName, nil
}

func restoreWithStoppedContainers(ctx context.Context, runtime daemonRuntime, cfg config.Config, profileName, appName, sourceID, snapshotID string, skipPreBackup bool, selected restoreCandidate, stopped []string) error {
	profileCfg := cfg.Profiles[profileName]
	if !skipPreBackup {
		if err := daemon.LoadExcludeFiles("/etc/volust/excludes", &selected.Spec); err != nil {
			return restartContainers(ctx, runtime, stopped, err)
		}
	}

	var commands []volustdocker.WorkerCommand
	var mounts []volustdocker.JobMount
	mounts = append(mounts,
		volustdocker.BuildSourceMount(selected.Source, restic.SourcePath(selected.Spec, selected.Source), false),
		volustdocker.BuildSourceMount(selected.Source, "/volust/target", false),
		volustdocker.JobMount{Type: "volume", Target: "/volust/staging"},
	)
	if !skipPreBackup {
		backup := restic.BackupCommand(profileCfg, selected.Spec, selected.Source, nil)
		commands = append(commands, volustdocker.WorkerCommand{Operation: backup.Operation, Args: backup.Args, Env: backup.Env})
	}
	command := restic.RestoreCommand(cfg.Profiles[profileName], restic.RestoreRequest{
		SnapshotID: snapshotID,
		App:        appName,
		Container:  selected.Spec.ContainerName,
		Profile:    profileName,
		SourceID:   sourceID,
		TargetPath: "/volust/target",
	})
	commands = append(commands, volustdocker.WorkerCommand{Operation: command.Operation, Args: command.Args, Env: command.Env})
	worker := volustdocker.WorkerSpec{
		Name:     volustdocker.WorkerName("restore", appName+"-"+sourceID),
		Image:    workerImage(),
		Env:      profileCfg.ResticEnv(),
		Mounts:   mounts,
		Commands: commands,
	}
	if err := runtime.RunWorker(ctx, worker); err != nil {
		return restartContainers(ctx, runtime, stopped, err)
	}
	return restartContainers(ctx, runtime, stopped, nil)
}

func resolveBackupSelection(ctx context.Context, runtime daemonRuntime, cfg config.Config, profileName, appName, sourceID string, reader *bufio.Reader, out io.Writer) ([]restoreCandidate, error) {
	candidates, err := restoreCandidates(ctx, runtime, cfg, profileName)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no backup candidates found for profile=%s", profileName)
	}

	if appName == "" {
		appName, err = promptChoice(reader, out, "Select application", uniqueApps(candidates))
		if err != nil {
			return nil, err
		}
	}
	appCandidates := filterCandidates(candidates, func(candidate restoreCandidate) bool {
		return candidate.Spec.Name == appName
	})
	if len(appCandidates) == 0 {
		return nil, fmt.Errorf("backup app not found for profile=%s app=%s", profileName, appName)
	}
	if sourceID == "" {
		return appCandidates, nil
	}
	sourceCandidates := filterCandidates(appCandidates, func(candidate restoreCandidate) bool {
		return candidate.Source.ID == sourceID
	})
	if len(sourceCandidates) == 0 {
		return nil, fmt.Errorf("backup source not found for profile=%s app=%s source=%s", profileName, appName, sourceID)
	}
	return sourceCandidates, nil
}

func resolveSnapshot(ctx context.Context, runtime daemonRuntime, profile config.Profile, request restic.RestoreRequest) (string, error) {
	snapshots, err := querySnapshots(ctx, runtime, profile, request)
	if err != nil {
		return "", err
	}
	if request.SnapshotID != "latest" {
		if len(snapshots) == 0 {
			return "", fmt.Errorf("snapshot %s not found for app=%s profile=%s source=%s", request.SnapshotID, request.App, request.Profile, request.SourceID)
		}
		return request.SnapshotID, nil
	}
	snapshot, ok := restic.LatestSnapshot(snapshots, request.App, request.Container, request.Profile, request.SourceID)
	if !ok || snapshot.SnapshotID() == "" {
		return "", fmt.Errorf("no matching snapshot found for app=%s profile=%s source=%s", request.App, request.Profile, request.SourceID)
	}
	return snapshot.SnapshotID(), nil
}

func querySnapshots(ctx context.Context, runtime daemonRuntime, profile config.Profile, request restic.RestoreRequest) ([]restic.Snapshot, error) {
	command := restic.SnapshotsCommand(profile, request)
	worker := volustdocker.WorkerSpec{
		Name:  volustdocker.WorkerName("snapshots", request.App+"-"+request.SourceID),
		Image: workerImage(),
		Env:   profile.ResticEnv(),
		Commands: []volustdocker.WorkerCommand{{
			Operation: command.Operation,
			Args:      command.Args,
			Env:       command.Env,
		}},
	}
	output, err := runtime.RunWorkerOutput(ctx, worker)
	if err != nil {
		return nil, err
	}
	var snapshots []restic.Snapshot
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, fmt.Errorf("snapshots output is not valid JSON: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return snapshots, nil
}

func stopMountedContainers(ctx context.Context, runtime daemonRuntime, selected restoreCandidate) ([]string, error) {
	containers, err := runtime.ListContainers(ctx, volustdocker.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var stopped []string
	for _, container := range containers {
		if !container.Running || container.ID == "" {
			continue
		}
		if !usesSource(container, selected.Source) {
			continue
		}
		if err := runtime.StopContainer(ctx, container.ID); err != nil {
			return stopped, restartContainers(ctx, runtime, stopped, err)
		}
		stopped = append(stopped, container.ID)
	}
	return stopped, nil
}

func usesSource(container volustdocker.Container, source volustdocker.Source) bool {
	for _, mount := range container.Mounts {
		if mount.Type != source.Type {
			continue
		}
		if source.Type == "volume" && mount.Name != "" && mount.Name == source.VolumeName {
			return true
		}
		if source.Type != "volume" && mount.Source != "" && mount.Source == source.HostSource {
			return true
		}
	}
	return false
}

func restartContainers(_ context.Context, runtime daemonRuntime, ids []string, err error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var startErrs []error
	for i := len(ids) - 1; i >= 0; i-- {
		if startErr := runtime.StartContainer(cleanupCtx, ids[i]); startErr != nil {
			startErrs = append(startErrs, startErr)
		}
	}
	if err != nil && len(startErrs) > 0 {
		return errors.Join(append([]error{err}, startErrs...)...)
	}
	if err != nil {
		return err
	}
	return errors.Join(startErrs...)
}

type restoreCandidate struct {
	Spec    volustdocker.BackupSpec
	Source  volustdocker.Source
	Running bool
}

func resolveRestoreSelection(ctx context.Context, runtime daemonRuntime, cfg config.Config, profileName, appName, sourceID string, reader *bufio.Reader, out io.Writer) (restoreCandidate, error) {
	candidates, err := restoreCandidates(ctx, runtime, cfg, profileName)
	if err != nil {
		return restoreCandidate{}, err
	}
	if len(candidates) == 0 {
		return restoreCandidate{}, fmt.Errorf("no restore candidates found for profile=%s", profileName)
	}

	if appName == "" {
		appName, err = promptChoice(reader, out, "Select application", uniqueApps(candidates))
		if err != nil {
			return restoreCandidate{}, err
		}
	}
	appCandidates := filterCandidates(candidates, func(candidate restoreCandidate) bool {
		return candidate.Spec.Name == appName
	})
	if len(appCandidates) == 0 {
		return restoreCandidate{}, fmt.Errorf("restore app not found for profile=%s app=%s", profileName, appName)
	}

	if sourceID == "" {
		sourceID, err = promptChoice(reader, out, "Select source", uniqueSources(appCandidates))
		if err != nil {
			return restoreCandidate{}, err
		}
	}
	for _, candidate := range appCandidates {
		if candidate.Source.ID == sourceID {
			return candidate, nil
		}
	}
	return restoreCandidate{}, fmt.Errorf("restore source not found for profile=%s app=%s source=%s", profileName, appName, sourceID)
}

func resolveAllVolumeRestoreSelection(ctx context.Context, runtime daemonRuntime, cfg config.Config, profileName string) ([]restoreCandidate, error) {
	candidates, err := restoreCandidates(ctx, runtime, cfg, profileName)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no restore candidates found for profile=%s", profileName)
	}
	seen := map[string]bool{}
	var selected []restoreCandidate
	for _, candidate := range candidates {
		if candidate.Source.Type != "volume" || candidate.Source.VolumeName == "" {
			continue
		}
		key := restoreSnapshotKey(candidate)
		if seen[key] {
			continue
		}
		seen[key] = true
		selected = append(selected, candidate)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no volume restore candidates found for profile=%s", profileName)
	}
	return selected, nil
}

func restoreCandidates(ctx context.Context, runtime daemonRuntime, cfg config.Config, profileName string) ([]restoreCandidate, error) {
	containers, err := runtime.ListContainers(ctx, volustdocker.ListOptions{IncludeStopped: includeStoppedContainers()})
	if err != nil {
		return nil, err
	}
	var candidates []restoreCandidate
	for _, container := range containers {
		spec, err := volustdocker.ParseBackupSpecWithDefaults(container, cfg.Profiles, cfg.Defaults)
		if err != nil || spec.Profile != profileName {
			continue
		}
		for _, source := range spec.Sources {
			candidates = append(candidates, restoreCandidate{Spec: spec, Source: source, Running: container.Running})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Spec.Name == candidates[j].Spec.Name {
			return candidates[i].Source.ID < candidates[j].Source.ID
		}
		return candidates[i].Spec.Name < candidates[j].Spec.Name
	})
	return candidates, nil
}

func promptChoice(reader *bufio.Reader, out io.Writer, title string, values []string) (string, error) {
	if len(values) == 1 {
		fmt.Fprintf(out, "%s: %s\n", title, values[0])
		return values[0], nil
	}
	fmt.Fprintf(out, "%s:\n", title)
	for i, value := range values {
		fmt.Fprintf(out, "  %d) %s\n", i+1, value)
	}
	fmt.Fprint(out, "> ")
	answer, _ := reader.ReadString('\n')
	index, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || index < 1 || index > len(values) {
		return "", fmt.Errorf("invalid selection %q", strings.TrimSpace(answer))
	}
	return values[index-1], nil
}

func uniqueApps(candidates []restoreCandidate) []string {
	seen := map[string]bool{}
	var values []string
	for _, candidate := range candidates {
		if !seen[candidate.Spec.Name] {
			seen[candidate.Spec.Name] = true
			values = append(values, candidate.Spec.Name)
		}
	}
	return values
}

func uniqueSources(candidates []restoreCandidate) []string {
	seen := map[string]bool{}
	var values []string
	for _, candidate := range candidates {
		if !seen[candidate.Source.ID] {
			seen[candidate.Source.ID] = true
			values = append(values, candidate.Source.ID)
		}
	}
	return values
}

func filterCandidates(candidates []restoreCandidate, keep func(restoreCandidate) bool) []restoreCandidate {
	var filtered []restoreCandidate
	for _, candidate := range candidates {
		if keep(candidate) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func workerImage() string {
	if image := os.Getenv("VOLUST_WORKER_IMAGE"); image != "" {
		return image
	}
	if image := os.Getenv("VOLUST_JOB_IMAGE"); image != "" {
		return image
	}
	return DefaultWorkerImage
}

func includeStoppedContainers() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("VOLUST_INCLUDE_STOPPED_CONTAINERS")))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func stopContainersBeforeBackup() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("VOLUST_STOP_CONTAINERS_BEFORE_BACKUP")))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func maxConcurrentWritesLimiter() (*daemon.WriteLimiter, error) {
	value := strings.TrimSpace(os.Getenv("VOLUST_MAX_CONCURRENT_WRITES"))
	if value == "" {
		return daemon.NewWriteLimiter(4), nil
	}
	max, err := strconv.Atoi(value)
	if err != nil {
		return nil, fmt.Errorf("VOLUST_MAX_CONCURRENT_WRITES must be an integer: %w", err)
	}
	if max < 0 {
		return nil, fmt.Errorf("VOLUST_MAX_CONCURRENT_WRITES must be >= 0")
	}
	return daemon.NewWriteLimiter(max), nil
}

func usage(out io.Writer) error {
	fmt.Fprintln(out, "usage: volust <daemon|apps|snapshots|backup|restore> [options]")
	return errors.New("missing or unknown command")
}

func profileNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
