package restic

import (
	"sort"
	"strings"

	"github.com/monlor/volust/internal/config"
	"github.com/monlor/volust/internal/docker"
)

type Command struct {
	Operation string
	Args      []string
	Env       map[string]string
}

type RestoreRequest struct {
	SnapshotID string
	App        string
	Profile    string
	SourceID   string
	TargetPath string
}

type Snapshot struct {
	ID      string   `json:"id"`
	ShortID string   `json:"short_id"`
	Time    string   `json:"time"`
	Tags    []string `json:"tags"`
}

func BackupCommand(profile config.Profile, spec docker.BackupSpec, source docker.Source, excludeFiles []string) Command {
	backupArgs := baseArgs(profile, "backup")
	backupArgs = append(backupArgs, "/volust/sources/"+source.ID)
	backupArgs = append(backupArgs, tagArgs(spec, source.ID)...)
	for _, exclude := range spec.Excludes {
		backupArgs = append(backupArgs, "--exclude", exclude)
	}
	for _, excludeFile := range excludeFiles {
		backupArgs = append(backupArgs, "--exclude-file", excludeFile)
	}
	initArgs := baseArgs(profile, "init")
	script := shellJoin(initArgs) + " || true && " + shellJoin(backupArgs)
	return Command{Operation: "backup", Args: []string{"sh", "-ec", script}, Env: profile.ResticEnv()}
}

func ForgetCommand(profile config.Profile, spec docker.BackupSpec, source docker.Source) Command {
	args := baseArgs(profile, "forget")
	args = append(args, tagArgs(spec, source.ID)...)
	args = append(args, spec.Retention.Args()...)
	return Command{Operation: "forget", Args: args, Env: profile.ResticEnv()}
}

func PruneCommand(profile config.Profile) Command {
	return Command{Operation: "prune", Args: baseArgs(profile, "prune"), Env: profile.ResticEnv()}
}

func RestoreCommand(profile config.Profile, request RestoreRequest) Command {
	staging := "/volust/staging/restore"
	include := "/volust/sources/" + request.SourceID
	steps := []string{
		shellJoin([]string{"rm", "-rf", staging}),
		shellJoin([]string{"mkdir", "-p", staging, request.TargetPath}),
	}
	if request.SnapshotID != "latest" {
		steps = append(steps, shellJoin(snapshotValidationArgs(profile, request, include))+" | grep -q '\"id\"'")
	}
	steps = append(steps,
		shellJoin(restoreArgs(profile, request, staging, include)),
		shellJoin([]string{"rsync", "-aHAX", "--numeric-ids", "--delete", staging + include + "/", request.TargetPath + "/"}),
	)
	script := strings.Join(steps, " && ")
	return Command{Operation: "restore", Args: []string{"sh", "-ec", script}, Env: profile.ResticEnv()}
}

func SnapshotsCommand(profile config.Profile, request RestoreRequest) Command {
	include := "/volust/sources/" + request.SourceID
	args := snapshotValidationArgs(profile, request, include)
	return Command{Operation: "snapshots", Args: args, Env: profile.ResticEnv()}
}

func LatestSnapshot(snapshots []Snapshot, app, profile, source string) (Snapshot, bool) {
	required := map[string]bool{
		"volust":             false,
		"app:" + app:         false,
		"profile:" + profile: false,
		"source:" + source:   false,
	}

	var matches []Snapshot
	for _, snapshot := range snapshots {
		seen := map[string]bool{}
		for _, tag := range snapshot.Tags {
			seen[tag] = true
		}
		ok := true
		for tag := range required {
			if !seen[tag] {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, snapshot)
		}
	}
	if len(matches) == 0 {
		return Snapshot{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Time > matches[j].Time
	})
	return matches[0], true
}

func (s Snapshot) SnapshotID() string {
	if s.ShortID != "" {
		return s.ShortID
	}
	return s.ID
}

func baseArgs(profile config.Profile, operation string) []string {
	return []string{"restic", "-r", profile.RepositoryString(), "--retry-lock", "5m", operation}
}

func tagArgs(spec docker.BackupSpec, sourceID string) []string {
	tags := []string{"volust", "app:" + spec.Name, "profile:" + spec.Profile, "source:" + sourceID}
	args := make([]string, 0, len(tags)*2)
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	return args
}

func restoreArgs(profile config.Profile, request RestoreRequest, staging, include string) []string {
	args := []string{"restic", "-r", profile.RepositoryString(), "restore", request.SnapshotID, "--target", staging, "--include", include}
	args = withRetryLock(args)
	args = append(args, "--path", include)
	if request.App != "" {
		args = append(args, "--tag", "volust", "--tag", "app:"+request.App)
	}
	if request.Profile != "" {
		args = append(args, "--tag", "profile:"+request.Profile)
	}
	if request.SourceID != "" {
		args = append(args, "--tag", "source:"+request.SourceID)
	}
	return args
}

func snapshotValidationArgs(profile config.Profile, request RestoreRequest, include string) []string {
	args := []string{"restic", "-r", profile.RepositoryString(), "snapshots", request.SnapshotID, "--json", "--path", include}
	args = withRetryLock(args)
	if request.App != "" {
		args = append(args, "--tag", "volust", "--tag", "app:"+request.App)
	}
	if request.Profile != "" {
		args = append(args, "--tag", "profile:"+request.Profile)
	}
	if request.SourceID != "" {
		args = append(args, "--tag", "source:"+request.SourceID)
	}
	return args
}

func withRetryLock(args []string) []string {
	if len(args) < 4 {
		return args
	}
	output := append([]string{}, args[:3]...)
	output = append(output, "--retry-lock", "5m")
	output = append(output, args[3:]...)
	return output
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') &&
			!(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') &&
			!strings.ContainsRune("@%_+=:,./-", r)
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}
