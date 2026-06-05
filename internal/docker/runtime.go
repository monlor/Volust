package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const defaultDockerSocket = "/var/run/docker.sock"

var jobNameCounter uint64

type Runtime struct {
	client *http.Client
	host   string
}

func NewRuntime() (*Runtime, error) {
	return NewRuntimeWithSocket(defaultDockerSocket), nil
}

func NewRuntimeWithSocket(socketPath string) *Runtime {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Runtime{
		client: &http.Client{Transport: transport},
		host:   "http://docker",
	}
}

func (r *Runtime) ListContainers(ctx context.Context, options ListOptions) ([]Container, error) {
	filters := map[string][]string{}
	if !options.All {
		filters["label"] = []string{"volust.enabled=true"}
	}
	if !options.IncludeStopped {
		filters["status"] = []string{"running"}
	}
	filterBody, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}
	endpoint := r.host + "/containers/json?filters=" + url.QueryEscape(string(filterBody))
	var listed []dockerListedContainer
	if err := r.doJSON(ctx, http.MethodGet, endpoint, nil, &listed); err != nil {
		return nil, err
	}

	containers := make([]Container, 0, len(listed))
	for _, item := range listed {
		var inspected dockerInspect
		if err := r.doJSON(ctx, http.MethodGet, r.host+"/containers/"+item.ID+"/json", nil, &inspected); err != nil {
			return nil, err
		}
		containers = append(containers, fromInspect(inspected))
	}
	return containers, nil
}

func (r *Runtime) StopContainer(ctx context.Context, id string) error {
	return r.doJSON(ctx, http.MethodPost, r.host+"/containers/"+id+"/stop", nil, nil)
}

func (r *Runtime) StartContainer(ctx context.Context, id string) error {
	return r.doJSON(ctx, http.MethodPost, r.host+"/containers/"+id+"/start", nil, nil)
}

func (r *Runtime) RunWorker(ctx context.Context, worker WorkerSpec) error {
	_, err := r.runWorker(ctx, worker, false)
	return err
}

func (r *Runtime) RunWorkerOutput(ctx context.Context, worker WorkerSpec) ([]byte, error) {
	return r.runWorker(ctx, worker, true)
}

func (r *Runtime) runWorker(ctx context.Context, worker WorkerSpec, captureOutput bool) ([]byte, error) {
	if len(worker.Commands) == 0 {
		return nil, nil
	}
	payload := dockerCreateRequest{
		Image:      worker.Image,
		Entrypoint: []string{"sh"},
		Cmd:        []string{"-ec", "trap 'exit 0' TERM INT; while :; do sleep 3600; done"},
		Env:        envList(worker.Env),
		HostConfig: dockerHostConfig{
			AutoRemove: false,
			Mounts:     dockerMounts(worker.Mounts),
		},
	}
	var created dockerCreateResponse
	endpoint := r.host + "/containers/create?name=" + url.QueryEscape(uniqueJobName(worker.Name))
	if err := r.doJSON(ctx, http.MethodPost, endpoint, payload, &created); err != nil {
		return nil, err
	}
	defer r.removeContainer(context.Background(), created.ID)
	if err := r.doJSON(ctx, http.MethodPost, r.host+"/containers/"+created.ID+"/start", nil, nil); err != nil {
		return nil, err
	}

	var output []byte
	for i, command := range worker.Commands {
		stdout, err := r.execWorkerCommand(ctx, created.ID, worker, command)
		if err != nil {
			return nil, err
		}
		if captureOutput && i == len(worker.Commands)-1 {
			output = stdout
		}
	}
	return output, nil
}

func (r *Runtime) execWorkerCommand(ctx context.Context, containerID string, worker WorkerSpec, command WorkerCommand) ([]byte, error) {
	var created dockerExecCreateResponse
	payload := dockerExecCreateRequest{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          append([]string{}, command.Args...),
		Env:          envList(command.Env),
	}
	if err := r.doJSON(ctx, http.MethodPost, r.host+"/containers/"+containerID+"/exec", payload, &created); err != nil {
		return nil, err
	}
	startPayload := dockerExecStartRequest{Detach: false, Tty: false}
	data, err := r.rawJSON(ctx, http.MethodPost, r.host+"/exec/"+created.ID+"/start", startPayload)
	if err != nil {
		return nil, err
	}
	stdout, _, combined := demuxDockerStream(data)

	var inspected dockerExecInspectResponse
	if err := r.doJSON(ctx, http.MethodGet, r.host+"/exec/"+created.ID+"/json", nil, &inspected); err != nil {
		return nil, err
	}
	if inspected.ExitCode != 0 {
		return nil, fmt.Errorf("worker %s command %s exited with %d: %s", worker.Name, command.Operation, inspected.ExitCode, strings.TrimSpace(string(combined)))
	}
	return stdout, nil
}

func demuxDockerLogs(input []byte) []byte {
	stdout, stderr, _ := demuxDockerStream(input)
	if len(stderr) == 0 {
		return stdout
	}
	return append(stdout, stderr...)
}

func demuxDockerStream(input []byte) ([]byte, []byte, []byte) {
	var stdout []byte
	var stderr []byte
	var combined []byte
	original := input
	parsed := false
	for len(input) >= 8 && (input[0] == 1 || input[0] == 2) && input[1] == 0 && input[2] == 0 && input[3] == 0 {
		stream := input[0]
		size := int(input[4])<<24 | int(input[5])<<16 | int(input[6])<<8 | int(input[7])
		if size < 0 || len(input) < 8+size {
			return original, nil, original
		}
		chunk := input[8 : 8+size]
		combined = append(combined, chunk...)
		if stream == 1 {
			stdout = append(stdout, chunk...)
		} else {
			stderr = append(stderr, chunk...)
		}
		input = input[8+size:]
		parsed = true
	}
	if !parsed || len(input) != 0 {
		return original, nil, original
	}
	return stdout, stderr, combined
}

func uniqueJobName(base string) string {
	counter := atomic.AddUint64(&jobNameCounter, 1)
	return fmt.Sprintf("%s-%x-%x", base, time.Now().UnixNano(), counter)
}

func (r *Runtime) removeContainer(ctx context.Context, id string) {
	_, _ = r.raw(ctx, http.MethodDelete, r.host+"/containers/"+id+"?force=1&v=1", nil)
}

func (r *Runtime) doJSON(ctx context.Context, method, endpoint string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	respBody, err := r.raw(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if output == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, output)
}

func (r *Runtime) rawJSON(ctx context.Context, method, endpoint string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	return r.raw(ctx, method, endpoint, reader)
}

func (r *Runtime) raw(ctx context.Context, method, endpoint string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("docker api %s %s returned %s: %s", method, endpoint, resp.Status, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

func fromInspect(inspect dockerInspect) Container {
	mounts := make([]Mount, 0, len(inspect.Mounts))
	for _, mount := range inspect.Mounts {
		mounts = append(mounts, Mount{
			Type:        mount.Type,
			Name:        mount.Name,
			Source:      mount.Source,
			Destination: mount.Destination,
			ReadOnly:    !mount.RW,
		})
	}
	return Container{
		ID:      inspect.ID,
		Name:    inspect.Name,
		Running: inspect.State.Running,
		Labels:  inspect.Config.Labels,
		Mounts:  mounts,
	}
}

func dockerMounts(mounts []JobMount) []dockerMount {
	output := make([]dockerMount, 0, len(mounts))
	for _, item := range mounts {
		output = append(output, dockerMount{
			Type:     item.Type,
			Source:   item.Source,
			Target:   item.Target,
			ReadOnly: item.ReadOnly,
		})
	}
	return output
}

func envList(env map[string]string) []string {
	items := make([]string, 0, len(env))
	for key, value := range env {
		items = append(items, key+"="+value)
	}
	return items
}

func entrypointAndCmd(args []string) ([]string, []string) {
	if len(args) == 0 {
		return nil, nil
	}
	return []string{args[0]}, append([]string{}, args[1:]...)
}

type dockerListedContainer struct {
	ID string `json:"Id"`
}

type dockerInspect struct {
	ID     string        `json:"Id"`
	Name   string        `json:"Name"`
	Config dockerConfig  `json:"Config"`
	State  dockerState   `json:"State"`
	Mounts []dockerMount `json:"Mounts"`
}

type dockerState struct {
	Running bool `json:"Running"`
}

type dockerConfig struct {
	Labels map[string]string `json:"Labels"`
}

type dockerCreateRequest struct {
	Image      string           `json:"Image"`
	Entrypoint []string         `json:"Entrypoint"`
	Cmd        []string         `json:"Cmd"`
	Env        []string         `json:"Env,omitempty"`
	HostConfig dockerHostConfig `json:"HostConfig"`
}

type dockerHostConfig struct {
	AutoRemove bool          `json:"AutoRemove"`
	Mounts     []dockerMount `json:"Mounts"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Target      string `json:"Target,omitempty"`
	Destination string `json:"Destination,omitempty"`
	Name        string `json:"Name,omitempty"`
	RW          bool   `json:"RW,omitempty"`
	ReadOnly    bool   `json:"ReadOnly,omitempty"`
}

type dockerCreateResponse struct {
	ID string `json:"Id"`
}

type dockerWaitResponse struct {
	StatusCode int64 `json:"StatusCode"`
}

type dockerExecCreateRequest struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
}

type dockerExecStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

type dockerExecCreateResponse struct {
	ID string `json:"Id"`
}

type dockerExecInspectResponse struct {
	ExitCode int `json:"ExitCode"`
}
