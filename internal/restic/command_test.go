package restic

import (
	"strings"
	"testing"

	"github.com/monlor/volust/internal/config"
	"github.com/monlor/volust/internal/docker"
	"github.com/monlor/volust/internal/policy"
)

func TestBackupForgetPruneCommands(t *testing.T) {
	retention, err := policy.ParseRetention("keep-last=7,keep-daily=7")
	if err != nil {
		t.Fatal(err)
	}
	spec := docker.BackupSpec{
		Name:          "postgres",
		ContainerName: "postgres-1",
		Profile:       "s3prod",
		Retention:     retention,
		Excludes:      []string{"cache/**"},
		Sources: []docker.Source{
			{ID: "data", ContainerPath: "/data"},
		},
	}
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app", Password: "secret"}

	backup := BackupCommand(profile, spec, spec.Sources[0], []string{"/etc/volust/excludes/common.txt"})
	wantBackup := []string{
		"sh", "-ec",
		"restic -r s3:s3.amazonaws.com/bucket/app --retry-lock 6h cat config >/dev/null 2>&1 || restic -r s3:s3.amazonaws.com/bucket/app --retry-lock 6h init && restic -r s3:s3.amazonaws.com/bucket/app --retry-lock 6h backup /volust/sources/postgres-1/data --tag volust --tag app:postgres --tag container:postgres-1 --tag profile:s3prod --tag source:data --exclude 'cache/**' --exclude-file /etc/volust/excludes/common.txt",
	}
	if !equalStrings(backup.Args, wantBackup) {
		t.Fatalf("backup args = %#v, want %#v", backup.Args, wantBackup)
	}
	if got := backup.Env["RESTIC_PASSWORD"]; got != "secret" {
		t.Fatalf("RESTIC_PASSWORD = %q", got)
	}

	forget := ForgetCommand(profile, spec, spec.Sources[0])
	wantForget := []string{
		"restic", "-r", "s3:s3.amazonaws.com/bucket/app", "--retry-lock", "6h", "forget",
		"--tag", "volust", "--tag", "app:postgres", "--tag", "container:postgres-1", "--tag", "profile:s3prod", "--tag", "source:data",
		"--keep-last", "7", "--keep-daily", "7",
	}
	if !equalStrings(forget.Args, wantForget) {
		t.Fatalf("forget args = %#v, want %#v", forget.Args, wantForget)
	}

	prune := PruneCommand(profile)
	wantPrune := []string{"restic", "-r", "s3:s3.amazonaws.com/bucket/app", "--retry-lock", "6h", "prune"}
	if !equalStrings(prune.Args, wantPrune) {
		t.Fatalf("prune args = %#v, want %#v", prune.Args, wantPrune)
	}
}

func TestRestoreCommandUsesStagingAndRsyncDelete(t *testing.T) {
	profile := config.Profile{Type: config.ProfileWebDAV, Path: "repo", Password: "secret"}
	cmd := RestoreCommand(profile, RestoreRequest{
		SnapshotID: "latest",
		App:        "postgres",
		Container:  "postgres-1",
		Profile:    "dav",
		SourceID:   "config",
		TargetPath: "/volust/target",
	})

	want := []string{
		"sh", "-ec",
		"rm -rf /volust/staging/restore && mkdir -p /volust/staging/restore /volust/target && restic -r rclone:volust_webdav:repo --retry-lock 6h restore latest --target /volust/staging/restore --include /volust/sources/postgres-1/config --path /volust/sources/postgres-1/config --tag volust --tag app:postgres --tag container:postgres-1 --tag profile:dav --tag source:config && rsync -aHAX --numeric-ids --delete /volust/staging/restore/volust/sources/postgres-1/config/ /volust/target/",
	}
	if !equalStrings(cmd.Args, want) {
		t.Fatalf("restore args = %#v, want %#v", cmd.Args, want)
	}
}

func TestRestoreCommandDoesNotRemoveStagingMountpoint(t *testing.T) {
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app"}
	cmd := RestoreCommand(profile, RestoreRequest{
		SnapshotID: "latest",
		App:        "postgres",
		Container:  "postgres-1",
		Profile:    "s3prod",
		SourceID:   "data",
		TargetPath: "/volust/target",
	})

	script := cmd.Args[2]
	if strings.Contains(script, "rm -rf /volust/staging &&") {
		t.Fatalf("restore script removes the staging mountpoint: %q", script)
	}
	if !strings.Contains(script, "rm -rf /volust/staging/restore") {
		t.Fatalf("restore script does not clear a child staging directory: %q", script)
	}
}

func TestRestoreCommandValidatesExplicitSnapshotBeforeDestructiveCopy(t *testing.T) {
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app"}
	cmd := RestoreCommand(profile, RestoreRequest{
		SnapshotID: "abc123",
		App:        "postgres",
		Container:  "postgres-1",
		Profile:    "s3prod",
		SourceID:   "data",
		TargetPath: "/volust/target",
	})

	script := cmd.Args[2]
	for _, want := range []string{
		"restic -r s3:s3.amazonaws.com/bucket/app --retry-lock 6h snapshots abc123 --json --path /volust/sources/postgres-1/data --tag volust --tag app:postgres --tag container:postgres-1 --tag profile:s3prod --tag source:data",
		"grep -q '\"id\"'",
		"restore abc123",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("restore script %q does not contain %q", script, want)
		}
	}
}

func TestSnapshotsCommandFiltersLatestByTags(t *testing.T) {
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app"}
	cmd := SnapshotsCommand(profile, RestoreRequest{
		SnapshotID: "latest",
		App:        "postgres",
		Container:  "postgres-1",
		Profile:    "s3prod",
		SourceID:   "data",
	})

	want := []string{
		"restic", "-r", "s3:s3.amazonaws.com/bucket/app", "--retry-lock", "6h", "snapshots", "latest",
		"--json", "--path", "/volust/sources/postgres-1/data",
		"--tag", "volust", "--tag", "app:postgres", "--tag", "container:postgres-1", "--tag", "profile:s3prod", "--tag", "source:data",
	}
	if !equalStrings(cmd.Args, want) {
		t.Fatalf("snapshot args = %#v, want %#v", cmd.Args, want)
	}
	if cmd.Operation != "snapshots" {
		t.Fatalf("operation = %q", cmd.Operation)
	}
}

func TestListCommandUsesResolvedSnapshot(t *testing.T) {
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app"}
	cmd := ListCommand(profile, "abc123")
	want := []string{"restic", "-r", "s3:s3.amazonaws.com/bucket/app", "--retry-lock", "6h", "ls", "abc123"}
	if !equalStrings(cmd.Args, want) {
		t.Fatalf("ls args = %#v, want %#v", cmd.Args, want)
	}
	if cmd.Operation != "ls" {
		t.Fatalf("operation = %q", cmd.Operation)
	}
}

func TestRestoreCommandQuotesShellArguments(t *testing.T) {
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app repo"}
	cmd := RestoreCommand(profile, RestoreRequest{
		SnapshotID: "latest",
		App:        "my app",
		Container:  "my app-1",
		Profile:    "prod",
		SourceID:   "config data",
		TargetPath: "/volust/target path",
	})

	script := cmd.Args[2]
	for _, want := range []string{
		"-r 's3:s3.amazonaws.com/bucket/app repo'",
		"--include '/volust/sources/my app-1/config data'",
		"--tag 'app:my app'",
		"mkdir -p /volust/staging/restore '/volust/target path'",
		"'/volust/staging/restore/volust/sources/my app-1/config data/' '/volust/target path/'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("restore script %q does not contain %q", script, want)
		}
	}
}

func TestCommandsUseConfiguredLockTimeout(t *testing.T) {
	t.Setenv("VOLUST_LOCK_TIMEOUT", "30m")
	profile := config.Profile{Type: config.ProfileS3, Repository: "s3:s3.amazonaws.com/bucket/app"}
	cmd := SnapshotsCommand(profile, RestoreRequest{
		SnapshotID: "latest",
		App:        "postgres",
		Container:  "postgres-1",
		Profile:    "s3prod",
		SourceID:   "data",
	})
	if got := strings.Join(cmd.Args, " "); !strings.Contains(got, "--retry-lock 30m") {
		t.Fatalf("snapshot command = %q", got)
	}
}

func TestLatestSnapshotSelectsNewestMatchingTags(t *testing.T) {
	snapshots := []Snapshot{
		{ID: "old", Time: "2026-01-01T00:00:00Z", Tags: []string{"volust", "app:postgres", "container:postgres-1", "profile:s3prod", "source:data"}},
		{ID: "other", Time: "2026-02-01T00:00:00Z", Tags: []string{"volust", "app:postgres", "container:postgres-2", "profile:s3prod", "source:data"}},
		{ID: "new", Time: "2026-03-01T00:00:00Z", Tags: []string{"volust", "app:postgres", "container:postgres-1", "profile:s3prod", "source:data"}},
	}

	got, ok := LatestSnapshot(snapshots, "postgres", "postgres-1", "s3prod", "data")
	if !ok {
		t.Fatal("LatestSnapshot returned no match")
	}
	if got.ID != "new" {
		t.Fatalf("LatestSnapshot ID = %q", got.ID)
	}
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
