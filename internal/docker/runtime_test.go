package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEntrypointAndCmdOverrideImageEntrypoint(t *testing.T) {
	entrypoint, cmd := entrypointAndCmd([]string{"restic", "-r", "repo", "backup", "/data"})
	if len(entrypoint) != 1 || entrypoint[0] != "restic" {
		t.Fatalf("entrypoint = %#v", entrypoint)
	}
	wantCmd := []string{"-r", "repo", "backup", "/data"}
	if !equalStrings(cmd, wantCmd) {
		t.Fatalf("cmd = %#v, want %#v", cmd, wantCmd)
	}
}

func TestRunJobFailureIncludesLogsAndDeletesContainer(t *testing.T) {
	var autoRemove bool
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/create"):
			var request dockerCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			autoRemove = request.HostConfig.AutoRemove
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "job123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/wait":
			_ = json.NewEncoder(w).Encode(dockerWaitResponse{StatusCode: 1})
		case r.Method == http.MethodGet && r.URL.Path == "/containers/job123/logs":
			_, _ = w.Write([]byte("restic failed"))
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/job123":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	err := runtime.RunJob(context.Background(), JobSpec{
		Name:  "backup",
		Image: "volust:latest",
		Args:  []string{"restic", "backup"},
	})
	if err == nil {
		t.Fatal("RunJob succeeded for failing container")
	}
	if autoRemove {
		t.Fatal("RunJob created job with AutoRemove enabled")
	}
	if !strings.Contains(err.Error(), "restic failed") {
		t.Fatalf("RunJob error did not include logs: %v", err)
	}
	if !deleted {
		t.Fatal("RunJob did not delete failed container after reading logs")
	}
}

func TestRunJobForceRemovesContainerWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var deleteForce string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "job123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/start":
			w.WriteHeader(http.StatusNoContent)
			cancel()
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/wait":
			<-r.Context().Done()
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/job123":
			deleteForce = r.URL.Query().Get("force")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	err := runtime.RunJob(ctx, JobSpec{
		Name:  "backup",
		Image: "volust:latest",
		Args:  []string{"restic", "backup"},
	})
	if err == nil {
		t.Fatal("RunJob succeeded after context cancellation")
	}
	if deleteForce != "1" {
		t.Fatalf("delete force query = %q", deleteForce)
	}
}

func TestRunJobCreatesContainerWithUniqueName(t *testing.T) {
	var createdName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/create"):
			createdName = r.URL.Query().Get("name")
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "job123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/wait":
			_ = json.NewEncoder(w).Encode(dockerWaitResponse{StatusCode: 0})
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/job123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	if err := runtime.RunJob(context.Background(), JobSpec{
		Name:  "backup",
		Image: "volust:latest",
		Args:  []string{"restic", "backup"},
	}); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}
	if createdName == "backup" || !strings.HasPrefix(createdName, "backup-") {
		t.Fatalf("created name = %q", createdName)
	}
}

func TestRunJobOutputCapturesOnlyStdoutOnSuccess(t *testing.T) {
	var logsQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "job123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/job123/wait":
			_ = json.NewEncoder(w).Encode(dockerWaitResponse{StatusCode: 0})
		case r.Method == http.MethodGet && r.URL.Path == "/containers/job123/logs":
			logsQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`[{"id":"snap"}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/job123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	output, err := runtime.RunJobOutput(context.Background(), JobSpec{
		Name:  "snapshots",
		Image: "volust:latest",
		Args:  []string{"restic", "snapshots", "--json"},
	})
	if err != nil {
		t.Fatalf("RunJobOutput returned error: %v", err)
	}
	if string(output) != `[{"id":"snap"}]` {
		t.Fatalf("RunJobOutput output = %q", output)
	}
	if !strings.Contains(logsQuery, "stdout=1") || strings.Contains(logsQuery, "stderr=1") {
		t.Fatalf("logs query = %q", logsQuery)
	}
}

func TestDemuxDockerLogsSupportsRawAndFramedOutput(t *testing.T) {
	if got := string(demuxDockerLogs([]byte("plain\n"))); got != "plain\n" {
		t.Fatalf("raw logs = %q", got)
	}
	framed := []byte{1, 0, 0, 0, 0, 0, 0, 6}
	framed = append(framed, []byte("hello\n")...)
	if got := string(demuxDockerLogs(framed)); got != "hello\n" {
		t.Fatalf("framed logs = %q", got)
	}
}

func TestListContainersFiltersStoppedByDefault(t *testing.T) {
	var filters string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			filters = r.URL.Query().Get("filters")
			_, _ = w.Write([]byte("[]"))
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	if _, err := runtime.ListContainers(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("ListContainers returned error: %v", err)
	}
	if !strings.Contains(filters, "status") || !strings.Contains(filters, "running") {
		t.Fatalf("filters = %q", filters)
	}
}

func TestListContainersCanIncludeStoppedContainers(t *testing.T) {
	var filters string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			filters = r.URL.Query().Get("filters")
			_ = json.NewEncoder(w).Encode([]dockerListedContainer{{ID: "app123"}})
		case r.Method == http.MethodGet && r.URL.Path == "/containers/app123/json":
			_ = json.NewEncoder(w).Encode(dockerInspect{
				ID:     "app123",
				Config: dockerConfig{Labels: map[string]string{"volust.enabled": "true"}},
				State:  dockerState{Running: false},
			})
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	containers, err := runtime.ListContainers(context.Background(), ListOptions{IncludeStopped: true})
	if err != nil {
		t.Fatalf("ListContainers returned error: %v", err)
	}
	if strings.Contains(filters, "status") {
		t.Fatalf("filters should not include status when stopped containers are included: %q", filters)
	}
	if len(containers) != 1 || containers[0].Running {
		t.Fatalf("containers = %#v", containers)
	}
}

func TestStopAndStartContainerCallDockerAPI(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	if err := runtime.StopContainer(context.Background(), "app123"); err != nil {
		t.Fatalf("StopContainer returned error: %v", err)
	}
	if err := runtime.StartContainer(context.Background(), "app123"); err != nil {
		t.Fatalf("StartContainer returned error: %v", err)
	}
	want := []string{"POST /containers/app123/stop", "POST /containers/app123/start"}
	if !equalStrings(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
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
