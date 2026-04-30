package docker

import "strings"

type JobRequest struct {
	Name   string
	Image  string
	Source Source
	Args   []string
	Env    map[string]string
}

type JobSpec struct {
	Name      string
	Image     string
	Operation string
	Args      []string
	Env       map[string]string
	Mounts    []JobMount
}

type JobMount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

func BuildBackupJob(request JobRequest) JobSpec {
	return buildJob("volust-backup-"+slug(request.Name), "backup", request, "/volust/sources/"+request.Source.ID, true)
}

func BuildRestoreJob(request JobRequest) JobSpec {
	job := buildJob("volust-restore-"+slug(request.Name), "restore", request, "/volust/target", false)
	job.Mounts = append(job.Mounts, JobMount{
		Type:   "volume",
		Target: "/volust/staging",
	})
	return job
}

func buildJob(name, operation string, request JobRequest, target string, readOnly bool) JobSpec {
	return JobSpec{
		Name:      name,
		Image:     request.Image,
		Operation: operation,
		Args:      append([]string{}, request.Args...),
		Env:       copyEnv(request.Env),
		Mounts: []JobMount{{
			Type:     request.Source.Type,
			Source:   mountSource(request.Source),
			Target:   target,
			ReadOnly: readOnly,
		}},
	}
}

func mountSource(source Source) string {
	if source.Type == "volume" {
		return source.VolumeName
	}
	return source.HostSource
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
		return "job"
	}
	return input
}
