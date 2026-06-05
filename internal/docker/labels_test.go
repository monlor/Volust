package docker

import (
	"testing"

	"github.com/monlor/volust/internal/config"
)

var testDefaults = config.PolicyDefaults{Schedule: "0 3 * * *"}

func TestParseBackupSpecFromLabels(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		ID:   "abc123",
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":      "true",
			"volust.profile":      "s3prod",
			"volust.sources":      "/data,/config",
			"volust.schedule":     "0 3 * * *",
			"volust.retention":    "keep-last=7,keep-daily=7",
			"volust.exclude":      "cache/**,tmp/**",
			"volust.exclude-file": "media.txt,common.txt",
		},
		Mounts: []Mount{
			{Name: "pgdata", Destination: "/data", Type: "volume"},
			{Source: "/srv/postgres/config", Destination: "/config", Type: "bind"},
		},
	}, map[string]config.Profile{"s3prod": {Type: config.ProfileS3}}, testDefaults)
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}

	if spec.Name != "postgres" {
		t.Fatalf("Name = %q", spec.Name)
	}
	if len(spec.Sources) != 2 {
		t.Fatalf("sources length = %d", len(spec.Sources))
	}
	if got := spec.Sources[0].ID; got != "data" {
		t.Fatalf("first source id = %q", got)
	}
	if got := spec.Sources[1].HostSource; got != "/srv/postgres/config" {
		t.Fatalf("bind host source = %q", got)
	}
	if got := spec.ExcludeFiles; len(got) != 2 || got[0] != "media.txt" || got[1] != "common.txt" {
		t.Fatalf("exclude files = %#v", got)
	}
}

func TestParseBackupSpecRejectsMissingDeclaredSource(t *testing.T) {
	_, err := ParseBackupSpecWithDefaults(Container{
		Name: "/app",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.profile":  "s3prod",
			"volust.sources":  "/missing",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []Mount{{Name: "data", Destination: "/data", Type: "volume"}},
	}, map[string]config.Profile{"s3prod": {Type: config.ProfileS3}}, testDefaults)
	if err == nil {
		t.Fatal("ParseBackupSpec succeeded for missing source mount")
	}
}

func TestParseBackupSpecDefaultsProfileAndName(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.sources":  "/data",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []Mount{{Name: "pgdata", Destination: "/data", Type: "volume"}},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, testDefaults)
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	if spec.Profile != "default" {
		t.Fatalf("profile = %q", spec.Profile)
	}
	if spec.Name != "postgres" {
		t.Fatalf("name = %q", spec.Name)
	}
	if spec.ContainerName != "postgres" {
		t.Fatalf("container name = %q", spec.ContainerName)
	}
}

func TestParseBackupSpecDefaultsSourcesFromBackupMounts(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []Mount{
			{Type: "volume", Name: "pgdata", Destination: "/data"},
			{Type: "bind", Source: "/srv/postgres/config", Destination: "/config"},
			{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
			{Type: "bind", Source: "/dev/kvm", Destination: "/dev/kvm"},
			{Type: "tmpfs", Destination: "/tmp"},
		},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, testDefaults)
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	if len(spec.Sources) != 2 {
		t.Fatalf("sources = %#v", spec.Sources)
	}
	if spec.Sources[0].ContainerPath != "/data" || spec.Sources[1].ContainerPath != "/config" {
		t.Fatalf("sources = %#v", spec.Sources)
	}
}

func TestParseBackupSpecDefaultsSkipAnonymousImageVolumes(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []Mount{
			{
				Type:        "volume",
				Name:        "1a6838d5b10dddd30dcbeb0ab209b89f3b320d1138e9b573ecc78bc0d1b822f8",
				Source:      "/var/lib/docker/volumes/1a6838d5b10dddd30dcbeb0ab209b89f3b320d1138e9b573ecc78bc0d1b822f8/_data",
				Destination: "/var/lib/postgresql",
			},
			{
				Type:        "volume",
				Name:        "saas_sub2api-postgres-data",
				Source:      "/var/lib/docker/volumes/saas_sub2api-postgres-data/_data",
				Destination: "/var/lib/postgresql/data",
			},
		},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, testDefaults)
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	if len(spec.Sources) != 1 || spec.Sources[0].ContainerPath != "/var/lib/postgresql/data" {
		t.Fatalf("sources = %#v", spec.Sources)
	}
}

func TestParseBackupSpecDefaultsKeepNamedNestedVolumes(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []Mount{
			{Type: "volume", Name: "postgres-root", Destination: "/var/lib/postgresql"},
			{Type: "volume", Name: "postgres-data", Destination: "/var/lib/postgresql/data"},
		},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, testDefaults)
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	if len(spec.Sources) != 2 {
		t.Fatalf("sources = %#v", spec.Sources)
	}
	if spec.Sources[0].ContainerPath != "/var/lib/postgresql" || spec.Sources[1].ContainerPath != "/var/lib/postgresql/data" {
		t.Fatalf("sources = %#v", spec.Sources)
	}
}

func TestParseBackupSpecUsesDefaultScheduleAndRetention(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled": "true",
			"volust.sources": "/data",
		},
		Mounts: []Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, config.PolicyDefaults{
		Schedule:  "0 3 * * *",
		Retention: "keep-last=7",
	})
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	if got := spec.Schedule.Expr; got != "0 3 * * *" {
		t.Fatalf("schedule = %q", got)
	}
	if got := spec.Retention.Args(); len(got) != 2 || got[0] != "--keep-last" || got[1] != "7" {
		t.Fatalf("retention args = %#v", got)
	}
}

func TestParseBackupSpecRetentionLabelOverridesDefaultButScheduleDoesNot(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":   "true",
			"volust.sources":   "/data",
			"volust.schedule":  "15 4 * * *",
			"volust.retention": "keep-last=3",
		},
		Mounts: []Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, config.PolicyDefaults{
		Schedule:  "0 3 * * *",
		Retention: "keep-last=7",
	})
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	if got := spec.Schedule.Expr; got != "0 3 * * *" {
		t.Fatalf("schedule = %q", got)
	}
	if got := spec.Retention.Args(); len(got) != 2 || got[1] != "3" {
		t.Fatalf("retention args = %#v", got)
	}
}

func TestParseBackupSpecParsesStopBeforeBackupLabel(t *testing.T) {
	spec, err := ParseBackupSpecWithDefaults(Container{
		Name: "/postgres",
		Labels: map[string]string{
			"volust.enabled":            "true",
			"volust.sources":            "/data",
			"volust.schedule":           "0 3 * * *",
			"volust.stop-before-backup": "yes",
		},
		Mounts: []Mount{{Type: "volume", Name: "pgdata", Destination: "/data"}},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, testDefaults)
	if err != nil {
		t.Fatalf("ParseBackupSpec returned error: %v", err)
	}
	if !spec.StopBeforeBackupSet || !spec.StopBeforeBackup {
		t.Fatalf("stop-before-backup not parsed: %#v", spec)
	}
}

func TestParseBackupSpecRejectsUnknownProfile(t *testing.T) {
	_, err := ParseBackupSpecWithDefaults(Container{
		Name: "/app",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.profile":  "missing",
			"volust.sources":  "/data",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []Mount{{Name: "data", Destination: "/data", Type: "volume"}},
	}, map[string]config.Profile{"s3prod": {Type: config.ProfileS3}}, testDefaults)
	if err == nil {
		t.Fatal("ParseBackupSpec succeeded for unknown profile")
	}
}

func TestParseBackupSpecRejectsDuplicateSourceIDs(t *testing.T) {
	_, err := ParseBackupSpecWithDefaults(Container{
		Name: "/app",
		Labels: map[string]string{
			"volust.enabled":  "true",
			"volust.sources":  "/a/b,/a-b",
			"volust.schedule": "0 3 * * *",
		},
		Mounts: []Mount{
			{Name: "slash", Destination: "/a/b", Type: "volume"},
			{Name: "dash", Destination: "/a-b", Type: "volume"},
		},
	}, map[string]config.Profile{"default": {Type: config.ProfileS3}}, testDefaults)
	if err == nil {
		t.Fatal("ParseBackupSpec succeeded for duplicate source IDs")
	}
}
