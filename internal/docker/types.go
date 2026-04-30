package docker

import (
	"fmt"
	"path"
	"strings"

	"github.com/monlor/volust/internal/config"
	"github.com/monlor/volust/internal/policy"
)

type Container struct {
	ID      string
	Name    string
	Running bool
	Labels  map[string]string
	Mounts  []Mount
}

type ListOptions struct {
	IncludeStopped bool
	All            bool
}

type Mount struct {
	Type        string
	Name        string
	Source      string
	Destination string
	ReadOnly    bool
}

type Source struct {
	ID            string
	ContainerPath string
	HostSource    string
	VolumeName    string
	Type          string
	ReadOnly      bool
}

type BackupSpec struct {
	ContainerID         string
	ContainerRunning    bool
	Name                string
	Profile             string
	Schedule            policy.Schedule
	Retention           policy.Retention
	StopBeforeBackup    bool
	StopBeforeBackupSet bool
	Excludes            []string
	ExcludeFiles        []string
	Sources             []Source
}

func ParseBackupSpec(container Container, profiles map[string]config.Profile) (BackupSpec, error) {
	return ParseBackupSpecWithDefaults(container, profiles, config.PolicyDefaults{})
}

func ParseBackupSpecWithDefaults(container Container, profiles map[string]config.Profile, defaults config.PolicyDefaults) (BackupSpec, error) {
	if !strings.EqualFold(container.Labels["volust.enabled"], "true") {
		return BackupSpec{}, fmt.Errorf("volust.enabled is not true")
	}
	profileName := strings.TrimSpace(container.Labels["volust.profile"])
	if profileName == "" {
		profileName = "default"
	}
	if _, ok := profiles[profileName]; !ok {
		return BackupSpec{}, fmt.Errorf("unknown profile %q", profileName)
	}
	sourcePaths := splitCSV(container.Labels["volust.sources"])
	if len(sourcePaths) == 0 {
		sourcePaths = defaultSourcePaths(container.Mounts)
	}
	if len(sourcePaths) == 0 {
		return BackupSpec{}, fmt.Errorf("no backup sources found")
	}
	scheduleExpr := defaultString(container.Labels["volust.schedule"], defaults.Schedule)
	schedule, err := policy.ParseSchedule(scheduleExpr)
	if err != nil {
		return BackupSpec{}, err
	}
	retentionExpr := defaultString(container.Labels["volust.retention"], defaults.Retention)
	retention, err := policy.ParseRetention(retentionExpr)
	if err != nil {
		return BackupSpec{}, err
	}
	stopBeforeBackup, stopBeforeBackupSet, err := parseOptionalBool(container.Labels["volust.stop-before-backup"])
	if err != nil {
		return BackupSpec{}, fmt.Errorf("volust.stop-before-backup: %w", err)
	}

	sources, err := resolveSources(sourcePaths, container.Mounts)
	if err != nil {
		return BackupSpec{}, err
	}
	name := strings.TrimSpace(container.Labels["volust.name"])
	if name == "" {
		name = strings.TrimPrefix(container.Name, "/")
	}

	return BackupSpec{
		ContainerID:         container.ID,
		ContainerRunning:    container.Running,
		Name:                name,
		Profile:             profileName,
		Schedule:            schedule,
		Retention:           retention,
		StopBeforeBackup:    stopBeforeBackup,
		StopBeforeBackupSet: stopBeforeBackupSet,
		Excludes:            splitCSV(container.Labels["volust.exclude"]),
		ExcludeFiles:        splitCSV(container.Labels["volust.exclude-file"]),
		Sources:             sources,
	}, nil
}

func defaultSourcePaths(mounts []Mount) []string {
	paths := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if !isBackupSourceMount(mount) {
			continue
		}
		paths = append(paths, path.Clean(mount.Destination))
	}
	return paths
}

func isBackupSourceMount(mount Mount) bool {
	destination := path.Clean(strings.TrimSpace(mount.Destination))
	source := strings.TrimSpace(mount.Source)
	if destination == "." || source == "" && mount.Name == "" {
		return false
	}
	if mount.Type != "" && mount.Type != "bind" && mount.Type != "volume" {
		return false
	}
	if isSocketPath(source) || isSocketPath(destination) {
		return false
	}
	return !isDeviceOrSystemPath(destination)
}

func isSocketPath(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "/var/run/docker.sock" || strings.HasSuffix(value, ".sock")
}

func isDeviceOrSystemPath(value string) bool {
	value = path.Clean(strings.TrimSpace(value))
	return value == "/dev" || strings.HasPrefix(value, "/dev/") ||
		value == "/proc" || strings.HasPrefix(value, "/proc/") ||
		value == "/sys" || strings.HasPrefix(value, "/sys/")
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func parseOptionalBool(value string) (bool, bool, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return false, false, nil
	}
	switch value {
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("must be a boolean value")
	}
}

func resolveSources(paths []string, mounts []Mount) ([]Source, error) {
	byDestination := map[string]Mount{}
	for _, mount := range mounts {
		byDestination[path.Clean(mount.Destination)] = mount
	}

	sources := make([]Source, 0, len(paths))
	for _, requested := range paths {
		clean := path.Clean(requested)
		mount, ok := byDestination[clean]
		if !ok {
			return nil, fmt.Errorf("source %q is not a mounted path", requested)
		}
		source := Source{
			ID:            sourceID(clean),
			ContainerPath: clean,
			HostSource:    mount.Source,
			VolumeName:    mount.Name,
			Type:          mount.Type,
			ReadOnly:      mount.ReadOnly,
		}
		for _, existing := range sources {
			if existing.ID == source.ID {
				return nil, fmt.Errorf("source %q conflicts with %q: duplicate source id %q", requested, existing.ContainerPath, source.ID)
			}
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func sourceID(containerPath string) string {
	id := strings.Trim(containerPath, "/")
	id = strings.ReplaceAll(id, "/", "-")
	if id == "" {
		return "root"
	}
	return id
}

func splitCSV(input string) []string {
	var values []string
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			values = append(values, part)
		}
	}
	return values
}
