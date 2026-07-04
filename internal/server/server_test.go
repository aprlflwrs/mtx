package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mtx/internal/config"
	"mtx/internal/queue"
)

func newTestServer(t *testing.T, cfg config.Config) (*Server, string) {
	t.Helper()
	dir := t.TempDir()

	store, err := queue.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	mediaFile := filepath.Join(dir, "media", "show", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(mediaFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaFile, []byte("fake video"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg.PathMappings = []config.PathMapping{
		{From: "/data", To: filepath.Join(dir, "media")},
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return &Server{Config: cfg, Store: store, Log: log}, mediaFile
}

func sonarrDownloadEvent() string {
	return `{"eventType": "Download", "episodeFile": {"path": "/data/show/episode.mkv"}}`
}

func TestWebhookEnqueuesImportedFileWithMappedPath(t *testing.T) {
	srv, _ := newTestServer(t, config.Defaults())

	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/webhook/sonarr", strings.NewReader(sonarrDownloadEvent()))
	srv.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	job, err := srv.Store.ClaimNext()
	if err != nil || job == nil {
		t.Fatalf("no job queued: %v %v", job, err)
	}
	if !strings.HasSuffix(job.Path, "/media/show/episode.mkv") {
		t.Errorf("container path was not mapped to host path: %s", job.Path)
	}
}

func TestWebhookTestEventAnswersOKWithoutEnqueueing(t *testing.T) {
	srv, _ := newTestServer(t, config.Defaults())

	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/webhook/radarr", strings.NewReader(`{"eventType": "Test"}`))
	srv.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Errorf("test event answered %d, the *arr connection test would fail", response.Code)
	}
	if job, _ := srv.Store.ClaimNext(); job != nil {
		t.Errorf("test event queued a job: %+v", job)
	}
}

func TestTokenGuardsWebhooksWhenConfigured(t *testing.T) {
	cfg := config.Defaults()
	cfg.WebhookToken = "sekrit"
	srv, _ := newTestServer(t, cfg)
	handler := srv.Handler()

	cases := []struct {
		name       string
		decorate   func(*http.Request)
		wantStatus int
	}{
		{"no token", func(r *http.Request) {}, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.SetBasicAuth("mtx", "wrong") }, http.StatusUnauthorized},
		{"basic auth password", func(r *http.Request) { r.SetBasicAuth("mtx", "sekrit") }, http.StatusAccepted},
		{"header token", func(r *http.Request) { r.Header.Set("X-Mtx-Token", "sekrit") }, http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/webhook/sonarr", strings.NewReader(sonarrDownloadEvent()))
			tc.decorate(request)
			handler.ServeHTTP(response, request)
			if response.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, tc.wantStatus)
			}
		})
	}
}

func TestGenericEnqueueAndStatus(t *testing.T) {
	srv, mediaFile := newTestServer(t, config.Defaults())
	handler := srv.Handler()

	body := fmt.Sprintf(`{"path": %q, "grain": true}`, mediaFile)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/enqueue", strings.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue status = %d, body = %s", response.Code, response.Body)
	}
	job, err := srv.Store.ClaimNext()
	if err != nil || job == nil || !job.Grain {
		t.Fatalf("grain job not queued: %+v %v", job, err)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/status", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"running":1`) {
		t.Errorf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestEnqueueMissingFileIsRejected(t *testing.T) {
	srv, _ := newTestServer(t, config.Defaults())

	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/webhook/sonarr",
		strings.NewReader(`{"eventType": "Download", "episodeFile": {"path": "/data/show/ghost.mkv"}}`))
	srv.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing file answered %d, want 422", response.Code)
	}
}
