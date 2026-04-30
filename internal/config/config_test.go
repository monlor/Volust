package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExpandsEnvironmentAndBuildsRepositories(t *testing.T) {
	originalObscurePassword := obscurePassword
	obscurePassword = func(value string) (string, error) {
		return "obscured-" + value, nil
	}
	t.Cleanup(func() {
		obscurePassword = originalObscurePassword
	})

	t.Setenv("RESTIC_PASSWORD", "secret")
	t.Setenv("AWS_KEY", "key")
	t.Setenv("AWS_SECRET", "secret-key")
	t.Setenv("WEBDAV_USER", "alice")
	t.Setenv("WEBDAV_PASS", "dav-pass")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := []byte(`
profiles:
  s3prod:
    type: s3
    repository: s3:s3.amazonaws.com/bucket/app
    password: ${RESTIC_PASSWORD}
    env:
      AWS_ACCESS_KEY_ID: ${AWS_KEY}
      AWS_SECRET_ACCESS_KEY: ${AWS_SECRET}
  dav:
    type: webdav
    path: backups
    password: ${RESTIC_PASSWORD}
    webdav:
      url: https://dav.example.com/remote.php/dav/files/alice
      user: ${WEBDAV_USER}
      pass: ${WEBDAV_PASS}
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	s3 := cfg.Profiles["s3prod"]
	if got := s3.RepositoryString(); got != "s3:s3.amazonaws.com/bucket/app" {
		t.Fatalf("s3 repository = %q", got)
	}
	if got := s3.Password; got != "secret" {
		t.Fatalf("password = %q", got)
	}
	if got := s3.Env["AWS_ACCESS_KEY_ID"]; got != "key" {
		t.Fatalf("AWS_ACCESS_KEY_ID = %q", got)
	}

	dav := cfg.Profiles["dav"]
	if got := dav.RepositoryString(); got != "rclone:volust_dav:backups" {
		t.Fatalf("webdav repository = %q", got)
	}
	if got := dav.Env["RCLONE_CONFIG_VOLUST_DAV_TYPE"]; got != "webdav" {
		t.Fatalf("rclone type env = %q", got)
	}
	if got := dav.WebDAV.Pass; got != "dav-pass" {
		t.Fatalf("webdav pass = %q", got)
	}
	if got := dav.Env["RCLONE_CONFIG_VOLUST_DAV_PASS"]; got != "obscured-dav-pass" {
		t.Fatalf("rclone pass env = %q", got)
	}
}

func TestWebDAVRemoteNameIsSafeForRcloneEnvironmentVariables(t *testing.T) {
	originalObscurePassword := obscurePassword
	obscurePassword = func(value string) (string, error) {
		return "obscured-" + value, nil
	}
	t.Cleanup(func() {
		obscurePassword = originalObscurePassword
	})

	path := writeConfig(t, "profiles:\n  prod-dav.cn:\n    type: webdav\n    path: backups\n    password: secret\n    webdav:\n      url: https://dav.example.com\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	profile := cfg.Profiles["prod-dav.cn"]
	if got := profile.RepositoryString(); got != "rclone:volust_prod_dav_cn:backups" {
		t.Fatalf("repository = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_PROD_DAV_CN_TYPE"]; got != "webdav" {
		t.Fatalf("rclone env = %#v", profile.Env)
	}
}

func TestLoadDefaultUsesEnvironmentBackedGDriveProfile(t *testing.T) {
	credentials := `{"type":"service_account","client_email":"volust@example.iam.gserviceaccount.com"}`
	t.Setenv("VOLUST_PROFILE_TYPE", ProfileGDrive)
	t.Setenv("VOLUST_GDRIVE_PATH", "restic/repo")
	t.Setenv("VOLUST_GDRIVE_SERVICE_ACCOUNT_BASE64", base64.StdEncoding.EncodeToString([]byte(credentials)))
	t.Setenv("VOLUST_GDRIVE_ROOT_FOLDER_ID", "root-folder")
	t.Setenv("VOLUST_GDRIVE_TEAM_DRIVE", "shared-drive")
	t.Setenv("RESTIC_PASSWORD", "secret")

	cfg, err := LoadDefault("")
	if err != nil {
		t.Fatalf("LoadDefault returned error: %v", err)
	}

	profile := cfg.Profiles["default"]
	if got := profile.Type; got != ProfileGDrive {
		t.Fatalf("type = %q", got)
	}
	if got := profile.RepositoryString(); got != "rclone:volust_default:restic/repo" {
		t.Fatalf("repository = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DEFAULT_TYPE"]; got != "drive" {
		t.Fatalf("rclone type = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DEFAULT_SCOPE"]; got != "drive" {
		t.Fatalf("rclone scope = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DEFAULT_SERVICE_ACCOUNT_CREDENTIALS"]; got != credentials {
		t.Fatalf("service account credentials = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DEFAULT_ROOT_FOLDER_ID"]; got != "root-folder" {
		t.Fatalf("root folder id = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DEFAULT_TEAM_DRIVE"]; got != "shared-drive" {
		t.Fatalf("team drive = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DEFAULT_USE_TRASH"]; got != "false" {
		t.Fatalf("use trash = %q", got)
	}
}

func TestLoadBuildsGDriveProfileFromConfig(t *testing.T) {
	credentials := `{"type":"service_account","client_email":"volust@example.iam.gserviceaccount.com"}`
	t.Setenv("GDRIVE_CREDS", base64.StdEncoding.EncodeToString([]byte(credentials)))
	path := writeConfig(t, `
profiles:
  drive-prod:
    type: gdrive
    path: backups
    password: secret
    gdrive:
      service_account_base64: ${GDRIVE_CREDS}
      root_folder_id: folder-id
      use_trash: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	profile := cfg.Profiles["drive-prod"]
	if got := profile.RepositoryString(); got != "rclone:volust_drive_prod:backups" {
		t.Fatalf("repository = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DRIVE_PROD_TYPE"]; got != "drive" {
		t.Fatalf("rclone type = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DRIVE_PROD_SERVICE_ACCOUNT_CREDENTIALS"]; got != credentials {
		t.Fatalf("service account credentials = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DRIVE_PROD_ROOT_FOLDER_ID"]; got != "folder-id" {
		t.Fatalf("root folder id = %q", got)
	}
	if got := profile.Env["RCLONE_CONFIG_VOLUST_DRIVE_PROD_USE_TRASH"]; got != "true" {
		t.Fatalf("use trash = %q", got)
	}
}

func TestLoadRejectsInvalidGDriveServiceAccountBase64(t *testing.T) {
	t.Setenv("VOLUST_PROFILE_TYPE", ProfileGDrive)
	t.Setenv("VOLUST_GDRIVE_SERVICE_ACCOUNT_BASE64", "not base64")
	t.Setenv("RESTIC_PASSWORD", "secret")

	_, err := LoadDefault("")
	if err == nil {
		t.Fatal("LoadDefault succeeded with invalid gdrive service account base64")
	}
	if !strings.Contains(err.Error(), "service account base64") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsInvalidGDriveServiceAccountJSON(t *testing.T) {
	t.Setenv("VOLUST_PROFILE_TYPE", ProfileGDrive)
	t.Setenv("VOLUST_GDRIVE_SERVICE_ACCOUNT_BASE64", base64.StdEncoding.EncodeToString([]byte("not json")))
	t.Setenv("RESTIC_PASSWORD", "secret")

	_, err := LoadDefault("")
	if err == nil {
		t.Fatal("LoadDefault succeeded with invalid gdrive service account JSON")
	}
	if !strings.Contains(err.Error(), "service account json") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsUnknownProfileType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("profiles:\n  bad:\n    type: ftp\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded for unknown profile type")
	}
}

func TestLoadDefaultUsesEnvironmentBackedS3Profile(t *testing.T) {
	t.Setenv("VOLUST_S3_REPOSITORY", "s3:s3.amazonaws.com/bucket/app")
	t.Setenv("RESTIC_PASSWORD", "secret")
	t.Setenv("AWS_ACCESS_KEY_ID", "key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	t.Setenv("VOLUST_DEFAULT_SCHEDULE", "0 3 * * *")
	t.Setenv("VOLUST_DEFAULT_RETENTION", "keep-last=7")

	cfg, err := LoadDefault("")
	if err != nil {
		t.Fatalf("LoadDefault returned error: %v", err)
	}

	profile := cfg.Profiles["default"]
	if got := profile.Type; got != ProfileS3 {
		t.Fatalf("type = %q", got)
	}
	if got := profile.RepositoryString(); got != "s3:s3.amazonaws.com/bucket/app" {
		t.Fatalf("repository = %q", got)
	}
	if got := profile.Password; got != "secret" {
		t.Fatalf("password = %q", got)
	}
	if got := profile.Env["AWS_DEFAULT_REGION"]; got != "us-east-1" {
		t.Fatalf("AWS_DEFAULT_REGION = %q", got)
	}
	if got := cfg.Defaults.Schedule; got != "0 3 * * *" {
		t.Fatalf("default schedule = %q", got)
	}
	if got := cfg.Defaults.Retention; got != "keep-last=7" {
		t.Fatalf("default retention = %q", got)
	}
}

func TestLoadDefaultUsesConfigWhenPathProvided(t *testing.T) {
	t.Setenv("VOLUST_S3_REPOSITORY", "s3:s3.amazonaws.com/env/repo")
	t.Setenv("VOLUST_DEFAULT_SCHEDULE", "0 3 * * *")
	path := writeConfig(t, "profiles:\n  file:\n    type: s3\n    repository: s3:s3.amazonaws.com/file/repo\n    password: secret\n")

	cfg, err := LoadDefault(path)
	if err != nil {
		t.Fatalf("LoadDefault returned error: %v", err)
	}
	if _, ok := cfg.Profiles["file"]; !ok {
		t.Fatalf("config profiles = %#v", cfg.Profiles)
	}
	if _, ok := cfg.Profiles["default"]; ok {
		t.Fatalf("environment default profile should not be added when config path is provided")
	}
	if got := cfg.Defaults.Schedule; got != "0 3 * * *" {
		t.Fatalf("default schedule = %q", got)
	}
}

func TestLoadDefaultRejectsMissingResticPassword(t *testing.T) {
	t.Setenv("VOLUST_S3_REPOSITORY", "s3:s3.amazonaws.com/bucket/app")

	if _, err := LoadDefault(""); err == nil {
		t.Fatal("LoadDefault succeeded without a restic password")
	}
}

func TestLoadDefaultRejectsResticPasswordFile(t *testing.T) {
	t.Setenv("VOLUST_S3_REPOSITORY", "s3:s3.amazonaws.com/bucket/app")
	t.Setenv("RESTIC_PASSWORD_FILE", "/run/secrets/restic-password")

	_, err := LoadDefault("")
	if err == nil {
		t.Fatal("LoadDefault accepted RESTIC_PASSWORD_FILE")
	}
	if !strings.Contains(err.Error(), "RESTIC_PASSWORD_FILE is not supported") {
		t.Fatalf("LoadDefault error = %v", err)
	}
}

func TestLoadRejectsConfigProfileWithResticPasswordFile(t *testing.T) {
	path := writeConfig(t, "profiles:\n  file:\n    type: s3\n    repository: s3:s3.amazonaws.com/file/repo\n    env:\n      RESTIC_PASSWORD_FILE: /run/secrets/restic-password\n")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted RESTIC_PASSWORD_FILE")
	}
	if !strings.Contains(err.Error(), "RESTIC_PASSWORD_FILE is not supported") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadRejectsConfigProfileWithoutResticPassword(t *testing.T) {
	path := writeConfig(t, "profiles:\n  file:\n    type: s3\n    repository: s3:s3.amazonaws.com/file/repo\n")

	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded without a restic password")
	}
}

func TestLoadAcceptsConfigProfileWithResticPasswordEnv(t *testing.T) {
	path := writeConfig(t, "profiles:\n  file:\n    type: s3\n    repository: s3:s3.amazonaws.com/file/repo\n    env:\n      RESTIC_PASSWORD_COMMAND: cat /run/secrets/restic-password\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.Profiles["file"].Env["RESTIC_PASSWORD_COMMAND"]; got != "cat /run/secrets/restic-password" {
		t.Fatalf("RESTIC_PASSWORD_COMMAND = %q", got)
	}
}

func TestLoadPreservesLiteralDollarInConfigValues(t *testing.T) {
	path := writeConfig(t, "profiles:\n  file:\n    type: s3\n    repository: s3:s3.amazonaws.com/file/repo\n    password: pa$word\n    env:\n      AWS_SECRET_ACCESS_KEY: key$still-secret\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	profile := cfg.Profiles["file"]
	if got := profile.Password; got != "pa$word" {
		t.Fatalf("password = %q", got)
	}
	if got := profile.Env["AWS_SECRET_ACCESS_KEY"]; got != "key$still-secret" {
		t.Fatalf("AWS_SECRET_ACCESS_KEY = %q", got)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
