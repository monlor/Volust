package docker

import "strings"

type WorkerSpec struct {
	Name     string
	Image    string
	Env      map[string]string
	Mounts   []JobMount
	Commands []WorkerCommand
}

type WorkerCommand struct {
	Operation string
	Args      []string
	Env       map[string]string
}

type JobMount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

func BuildSourceMount(source Source, target string, readOnly bool) JobMount {
	return JobMount{
		Type:     source.Type,
		Source:   mountSource(source),
		Target:   target,
		ReadOnly: readOnly,
	}
}

func copyEnv(input map[string]string) map[string]string {
	output := map[string]string{}
	for key, value := range input {
		output[key] = value
	}
	return output
}

func slug(input string) string {
	input = strings.TrimSpace(strings.ToLower(input))
	replacer := strings.NewReplacer("/", "-", "_", "-", " ", "-")
	input = replacer.Replace(input)
	input = strings.Trim(input, "-")
	if input == "" {
		return "worker"
	}
	return input
}

func WorkerName(prefix, name string) string {
	return "volust-" + prefix + "-" + slug(name)
}

func mountSource(source Source) string {
	if source.Type == "volume" {
		return source.VolumeName
	}
	return source.HostSource
}
