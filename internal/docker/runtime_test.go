package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunWorkerFailureIncludesExecOutputAndDeletesContainer(t *testing.T) {
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
			if len(request.HostConfig.Mounts) != 1 || request.HostConfig.Mounts[0].Target != "/volust/sources/postgres-1/data" {
				t.Fatalf("mounts = %#v", request.HostConfig.Mounts)
			}
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "worker123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/worker123/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/worker123/exec":
			_ = json.NewEncoder(w).Encode(dockerExecCreateResponse{ID: "exec123"})
		case r.Method == http.MethodPost && r.URL.Path == "/exec/exec123/start":
			_, _ = w.Write([]byte("restic failed"))
		case r.Method == http.MethodGet && r.URL.Path == "/exec/exec123/json":
			_ = json.NewEncoder(w).Encode(dockerExecInspectResponse{ExitCode: 1})
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/worker123":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	err := runtime.RunWorker(context.Background(), WorkerSpec{
		Name:  "backup",
		Image: "volust:latest",
		Mounts: []JobMount{{
			Type:     "volume",
			Source:   "pgdata",
			Target:   "/volust/sources/postgres-1/data",
			ReadOnly: true,
		}},
		Commands: []WorkerCommand{{Operation: "backup", Args: []string{"restic", "backup"}}},
	})
	if err == nil {
		t.Fatal("RunWorker succeeded for failing command")
	}
	if autoRemove {
		t.Fatal("RunWorker created worker with AutoRemove enabled")
	}
	if !strings.Contains(err.Error(), "restic failed") {
		t.Fatalf("RunWorker error did not include exec output: %v", err)
	}
	if !deleted {
		t.Fatal("RunWorker did not delete worker after reading output")
	}
}

func TestRunWorkerForceRemovesContainerWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var deleteForce string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "worker123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/worker123/start":
			w.WriteHeader(http.StatusNoContent)
			cancel()
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/worker123":
			deleteForce = r.URL.Query().Get("force")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	err := runtime.RunWorker(ctx, WorkerSpec{
		Name:     "backup",
		Image:    "volust:latest",
		Commands: []WorkerCommand{{Operation: "backup", Args: []string{"restic", "backup"}}},
	})
	if err == nil {
		t.Fatal("RunWorker succeeded after context cancellation")
	}
	if deleteForce != "1" {
		t.Fatalf("delete force query = %q", deleteForce)
	}
}

func TestRunWorkerCreatesContainerWithUniqueNameAndExecsCommands(t *testing.T) {
	var createdName string
	execs := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/create"):
			createdName = r.URL.Query().Get("name")
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "worker123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/worker123/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/worker123/exec":
			execs++
			_ = json.NewEncoder(w).Encode(dockerExecCreateResponse{ID: "exec" + string(rune('0'+execs))})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/exec/exec") && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/exec/exec") && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(dockerExecInspectResponse{ExitCode: 0})
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/worker123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	if err := runtime.RunWorker(context.Background(), WorkerSpec{
		Name:  "backup",
		Image: "volust:latest",
		Commands: []WorkerCommand{
			{Operation: "backup", Args: []string{"restic", "backup"}},
			{Operation: "prune", Args: []string{"restic", "prune"}},
		},
	}); err != nil {
		t.Fatalf("RunWorker returned error: %v", err)
	}
	if createdName == "backup" || !strings.HasPrefix(createdName, "backup-") {
		t.Fatalf("created name = %q", createdName)
	}
	if execs != 2 {
		t.Fatalf("execs = %d", execs)
	}
}

func TestRunWorkerOutputCapturesOnlyStdoutOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(dockerCreateResponse{ID: "worker123"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/worker123/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/worker123/exec":
			_ = json.NewEncoder(w).Encode(dockerExecCreateResponse{ID: "exec123"})
		case r.Method == http.MethodPost && r.URL.Path == "/exec/exec123/start":
			_, _ = w.Write(frame(1, []byte(`[{"id":"snap"}]`)))
			_, _ = w.Write(frame(2, []byte("notice\n")))
		case r.Method == http.MethodGet && r.URL.Path == "/exec/exec123/json":
			_ = json.NewEncoder(w).Encode(dockerExecInspectResponse{ExitCode: 0})
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/worker123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected docker API call: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	runtime := &Runtime{client: server.Client(), host: server.URL}
	output, err := runtime.RunWorkerOutput(context.Background(), WorkerSpec{
		Name:     "snapshots",
		Image:    "volust:latest",
		Commands: []WorkerCommand{{Operation: "snapshots", Args: []string{"restic", "snapshots", "--json"}}},
	})
	if err != nil {
		t.Fatalf("RunWorkerOutput returned error: %v", err)
	}
	if string(output) != `[{"id":"snap"}]` {
		t.Fatalf("RunWorkerOutput output = %q", output)
	}
}

func TestDemuxDockerLogsSupportsRawAndFramedOutput(t *testing.T) {
	if got := string(demuxDockerLogs([]byte("plain\n"))); got != "plain\n" {
		t.Fatalf("raw logs = %q", got)
	}
	framed := frame(1, []byte("hello\n"))
	if got := string(demuxDockerLogs(framed)); got != "hello\n" {
		t.Fatalf("framed logs = %q", got)
	}
}

func frame(stream byte, payload []byte) []byte {
	return append([]byte{stream, 0, 0, 0, byte(len(payload) >> 24), byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}, payload...)
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
