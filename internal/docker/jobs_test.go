package docker

import (
	"testing"
)

func TestBuildBackupJobMountsSourceReadOnly(t *testing.T) {
	job := BuildBackupJob(JobRequest{
		Name:  "postgres-data",
		Image: "volust:latest",
		Source: Source{
			ID:         "data",
			Type:       "volume",
			VolumeName: "pgdata",
		},
		Args: []string{"restic", "backup"},
	})

	if job.Name != "volust-backup-postgres-data" {
		t.Fatalf("name = %q", job.Name)
	}
	if len(job.Mounts) != 1 {
		t.Fatalf("mount count = %d", len(job.Mounts))
	}
	mount := job.Mounts[0]
	if mount.Source != "pgdata" || mount.Target != "/volust/sources/data" || !mount.ReadOnly {
		t.Fatalf("mount = %#v", mount)
	}
}

func TestBuildRestoreJobMountsTargetWritableAndStagingOutsideOverlay(t *testing.T) {
	job := BuildRestoreJob(JobRequest{
		Name:  "postgres-config",
		Image: "volust:latest",
		Source: Source{
			ID:         "config",
			Type:       "bind",
			HostSource: "/srv/postgres/config",
		},
		Args: []string{"sh", "-ec", "restore"},
	})

	if job.Name != "volust-restore-postgres-config" {
		t.Fatalf("name = %q", job.Name)
	}
	if len(job.Mounts) != 2 {
		t.Fatalf("mount count = %d", len(job.Mounts))
	}
	mount := job.Mounts[0]
	if mount.Source != "/srv/postgres/config" || mount.Target != "/volust/target" || mount.ReadOnly {
		t.Fatalf("mount = %#v", mount)
	}
	staging := job.Mounts[1]
	if staging.Type != "volume" || staging.Source != "" || staging.Target != "/volust/staging" || staging.ReadOnly {
		t.Fatalf("staging mount = %#v", staging)
	}
}
