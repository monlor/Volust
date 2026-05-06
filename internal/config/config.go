package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

var obscurePassword = rcloneObscurePassword

const (
	ProfileS3     = "s3"
	ProfileWebDAV = "webdav"
)

type Config struct {
	Defaults PolicyDefaults     `yaml:"defaults"`
	Profiles map[string]Profile `yaml:"profiles"`
}

type PolicyDefaults struct {
	Schedule  string `yaml:"schedule"`
	Retention string `yaml:"retention"`
}

type Profile struct {
	Type       string            `yaml:"type"`
	Repository string            `yaml:"repository"`
	Path       string            `yaml:"path"`
	Password   string            `yaml:"password"`
	Env        map[string]string `yaml:"env"`
	WebDAV     WebDAVConfig      `yaml:"webdav"`
	remoteName string
}

type WebDAVConfig struct {
	URL    string `yaml:"url"`
	User   string `yaml:"user"`
	Pass   string `yaml:"pass"`
	Vendor string `yaml:"vendor"`
}

func LoadDefault(path string) (Config, error) {
	if strings.TrimSpace(path) != "" {
		return Load(path)
	}
	return FromEnv()
}

func Load(path string) (Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	cfg.applyEnvDefaults()
	for name, profile := range cfg.Profiles {
		profile.expandEnvRefs()
		if err := profile.validate(name); err != nil {
			return Config{}, err
		}
		if err := profile.normalize(name); err != nil {
			return Config{}, err
		}
		cfg.Profiles[name] = profile
	}
	return cfg, nil
}

func (p *Profile) expandEnvRefs() {
	p.Type = expandEnvRefs(p.Type)
	p.Repository = expandEnvRefs(p.Repository)
	p.Path = expandEnvRefs(p.Path)
	p.Password = expandEnvRefs(p.Password)
	for key, value := range p.Env {
		p.Env[key] = expandEnvRefs(value)
	}
	p.WebDAV.URL = expandEnvRefs(p.WebDAV.URL)
	p.WebDAV.User = expandEnvRefs(p.WebDAV.User)
	p.WebDAV.Pass = expandEnvRefs(p.WebDAV.Pass)
	p.WebDAV.Vendor = expandEnvRefs(p.WebDAV.Vendor)
}

func expandEnvRefs(input string) string {
	return envRefPattern.ReplaceAllStringFunc(input, func(match string) string {
		name := envRefPattern.FindStringSubmatch(match)[1]
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return match
	})
}

func FromEnv() (Config, error) {
	profileName := envDefault("VOLUST_PROFILE", "default")
	profileType := envDefault("VOLUST_PROFILE_TYPE", ProfileS3)
	profile := Profile{
		Type:     profileType,
		Password: os.Getenv("RESTIC_PASSWORD"),
		Env:      resticEnvFromOS(),
	}

	switch profileType {
	case ProfileS3:
		profile.Repository = firstEnv("VOLUST_S3_REPOSITORY", "VOLUST_REPOSITORY", "RESTIC_REPOSITORY")
	case ProfileWebDAV:
		profile.Path = envDefault("VOLUST_WEBDAV_PATH", "volust")
		profile.WebDAV = WebDAVConfig{
			URL:    os.Getenv("VOLUST_WEBDAV_URL"),
			User:   os.Getenv("VOLUST_WEBDAV_USER"),
			Pass:   os.Getenv("VOLUST_WEBDAV_PASS"),
			Vendor: os.Getenv("VOLUST_WEBDAV_VENDOR"),
		}
	default:
		return Config{}, fmt.Errorf("unsupported profile type %q", profileType)
	}
	if err := profile.validate(profileName); err != nil {
		return Config{}, err
	}
	if err := profile.normalize(profileName); err != nil {
		return Config{}, err
	}
	cfg := Config{Profiles: map[string]Profile{profileName: profile}}
	cfg.applyEnvDefaults()
	return cfg, nil
}

func (c *Config) applyEnvDefaults() {
	if c.Defaults.Schedule == "" {
		c.Defaults.Schedule = os.Getenv("VOLUST_DEFAULT_SCHEDULE")
	}
	if c.Defaults.Retention == "" {
		c.Defaults.Retention = os.Getenv("VOLUST_DEFAULT_RETENTION")
	}
}

func (p Profile) RepositoryString() string {
	switch p.Type {
	case ProfileWebDAV:
		return "rclone:" + p.rcloneRemoteName() + ":" + strings.TrimPrefix(p.Path, "/")
	default:
		return p.Repository
	}
}

func (p Profile) RepositoryStringForApp(appName string) string {
	appDir := AppRepositoryDir(appName)
	switch p.Type {
	case ProfileWebDAV:
		base := strings.Trim(strings.TrimPrefix(p.Path, "/"), "/")
		if base == "" {
			return "rclone:" + p.rcloneRemoteName() + ":" + appDir
		}
		return "rclone:" + p.rcloneRemoteName() + ":" + base + "/" + appDir
	default:
		return strings.TrimRight(p.Repository, "/") + "/" + appDir
	}
}

func (p Profile) BackendKey() string {
	switch p.Type {
	case ProfileWebDAV:
		return "webdav\x00" + normalizeBackendPart(p.WebDAV.URL)
	case ProfileS3:
		endpoint, bucket, ok := parseS3RepositoryRoot(p.Repository)
		if ok {
			return "s3\x00" + normalizeBackendPart(endpoint) + "\x00" + bucket
		}
		return "s3\x00" + normalizeBackendPart(strings.TrimRight(p.Repository, "/"))
	default:
		return p.Type + "\x00" + normalizeBackendPart(p.RepositoryString())
	}
}

func AppRepositoryDir(appName string) string {
	value := strings.TrimSpace(appName)
	if value == "" {
		value = "app"
	}
	if isSafeAppRepositoryDir(value) {
		return value
	}
	slug := appRepositorySlug(value)
	sum := sha256.Sum256([]byte(value))
	return slug + "-" + hex.EncodeToString(sum[:4])
}

func isSafeAppRepositoryDir(value string) bool {
	if value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

func appRepositorySlug(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(builder.String(), ".-_")
	if slug == "" {
		return "app"
	}
	return slug
}

func parseS3RepositoryRoot(repository string) (string, string, bool) {
	const prefix = "s3:"
	if !strings.HasPrefix(repository, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(repository, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func normalizeBackendPart(value string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(value), "/"))
}

func (p Profile) ResticEnv() map[string]string {
	env := map[string]string{}
	for key, value := range p.Env {
		env[key] = value
	}
	if p.Password != "" {
		env["RESTIC_PASSWORD"] = p.Password
	}
	return env
}

func (p Profile) validate(name string) error {
	if p.Env["RESTIC_PASSWORD_FILE"] != "" {
		return fmt.Errorf("profile %q: RESTIC_PASSWORD_FILE is not supported because job containers cannot access host-local password files; use RESTIC_PASSWORD or RESTIC_PASSWORD_COMMAND instead", name)
	}
	if !p.hasResticPassword() {
		return fmt.Errorf("profile %q: restic password is required", name)
	}
	switch p.Type {
	case ProfileS3:
		if p.Repository == "" {
			return fmt.Errorf("profile %q: repository is required for s3", name)
		}
	case ProfileWebDAV:
		if p.Path == "" {
			return fmt.Errorf("profile %q: path is required for webdav", name)
		}
		if p.WebDAV.URL == "" {
			return fmt.Errorf("profile %q: webdav.url is required", name)
		}
	default:
		return fmt.Errorf("profile %q: unsupported type %q", name, p.Type)
	}
	return nil
}

func (p Profile) hasResticPassword() bool {
	if p.Password != "" {
		return true
	}
	for _, key := range []string{"RESTIC_PASSWORD", "RESTIC_PASSWORD_COMMAND"} {
		if p.Env[key] != "" {
			return true
		}
	}
	return false
}

func (p *Profile) normalize(name string) error {
	if p.Env == nil {
		p.Env = map[string]string{}
	}
	switch p.Type {
	case ProfileWebDAV:
		return p.normalizeWebDAV(name)
	default:
		return nil
	}
}

func (p *Profile) normalizeWebDAV(name string) error {
	p.remoteName = "volust_" + rcloneSafeName(name)

	remote := strings.ToUpper(p.rcloneRemoteName())
	p.Env["RCLONE_CONFIG_"+remote+"_TYPE"] = "webdav"
	p.Env["RCLONE_CONFIG_"+remote+"_URL"] = p.WebDAV.URL
	if p.WebDAV.User != "" {
		p.Env["RCLONE_CONFIG_"+remote+"_USER"] = p.WebDAV.User
	}
	if p.WebDAV.Pass != "" {
		obscured, err := obscurePassword(p.WebDAV.Pass)
		if err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
		p.Env["RCLONE_CONFIG_"+remote+"_PASS"] = obscured
	}
	vendor := p.WebDAV.Vendor
	if vendor == "" {
		vendor = "other"
	}
	p.Env["RCLONE_CONFIG_"+remote+"_VENDOR"] = vendor
	return nil
}

func rcloneObscurePassword(value string) (string, error) {
	output, err := exec.Command("rclone", "obscure", value).Output()
	if err != nil {
		return "", fmt.Errorf("rclone obscure password: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (p Profile) rcloneRemoteName() string {
	if p.remoteName != "" {
		return p.remoteName
	}
	return "volust_" + rcloneSafeName(p.Type)
}

func rcloneSafeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastUnderscore := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		if valid {
			builder.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
	}
	output := strings.Trim(builder.String(), "_")
	if output == "" {
		return "remote"
	}
	return output
}

func resticEnvFromOS() map[string]string {
	env := map[string]string{}
	for _, key := range []string{
		"RESTIC_PASSWORD_FILE",
		"RESTIC_PASSWORD_COMMAND",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_DEFAULT_REGION",
		"AWS_REGION",
		"AWS_PROFILE",
		"AWS_SHARED_CREDENTIALS_FILE",
	} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	return env
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
