package docker

import "testing"

func TestBuildSourceMountForVolume(t *testing.T) {
	mount := BuildSourceMount(Source{
		ID:         "data",
		Type:       "volume",
		VolumeName: "pgdata",
	}, "/volust/sources/postgres-1/data", true)

	if mount.Type != "volume" || mount.Source != "pgdata" || mount.Target != "/volust/sources/postgres-1/data" || !mount.ReadOnly {
		t.Fatalf("mount = %#v", mount)
	}
}

func TestBuildSourceMountForBind(t *testing.T) {
	mount := BuildSourceMount(Source{
		ID:         "config",
		Type:       "bind",
		HostSource: "/srv/postgres/config",
	}, "/volust/target", false)

	if mount.Type != "bind" || mount.Source != "/srv/postgres/config" || mount.Target != "/volust/target" || mount.ReadOnly {
		t.Fatalf("mount = %#v", mount)
	}
}

func TestWorkerNameSlugsInput(t *testing.T) {
	if got := WorkerName("backup", "Team/Postgres Data"); got != "volust-backup-team-postgres-data" {
		t.Fatalf("worker name = %q", got)
	}
}
