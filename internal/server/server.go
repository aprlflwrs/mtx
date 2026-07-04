// Package server is the daemon's HTTP surface: webhook intake from the
// dockerized Sonarr/Radarr stack, generic enqueue, and status. The future
// web UI mounts here too.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"mtx/internal/config"
	"mtx/internal/queue"
)

type Server struct {
	Config config.Config
	Store  *queue.Store
	Log    *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/sonarr", s.requireToken(s.handleArrWebhook))
	mux.HandleFunc("POST /webhook/radarr", s.requireToken(s.handleArrWebhook))
	mux.HandleFunc("POST /enqueue", s.requireToken(s.handleEnqueue))
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// requireToken guards mutating endpoints when a webhook_token is configured.
// The token is accepted as a basic-auth password (what the *arr webhook
// connection settings offer) or an X-Mtx-Token header.
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Config.WebhookToken != "" && !s.presentsToken(r) {
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) presentsToken(r *http.Request) bool {
	_, password, _ := r.BasicAuth()
	for _, candidate := range []string{password, r.Header.Get("X-Mtx-Token")} {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(s.Config.WebhookToken)) == 1 {
			return true
		}
	}
	return false
}

// arrWebhook is the part of Sonarr's and Radarr's webhook payload we need:
// what happened, and which file landed.
type arrWebhook struct {
	EventType   string `json:"eventType"`
	EpisodeFile *struct {
		Path string `json:"path"`
	} `json:"episodeFile"`
	MovieFile *struct {
		Path string `json:"path"`
	} `json:"movieFile"`
}

func (p arrWebhook) importedFilePath() string {
	if p.EpisodeFile != nil {
		return p.EpisodeFile.Path
	}
	if p.MovieFile != nil {
		return p.MovieFile.Path
	}
	return ""
}

func (s *Server) handleArrWebhook(w http.ResponseWriter, r *http.Request) {
	var payload arrWebhook
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "unparseable webhook payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// "Test" is the *arr connection-test button; answering OK makes the
	// test pass. Only "Download" events (imports and upgrades) carry files.
	if payload.EventType != "Download" {
		s.Log.Info("webhook ignored", "event", payload.EventType)
		w.WriteHeader(http.StatusOK)
		return
	}
	containerPath := payload.importedFilePath()
	if containerPath == "" {
		http.Error(w, "Download event without a file path", http.StatusBadRequest)
		return
	}

	s.enqueue(w, s.mapToHostPath(containerPath), false)
}

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path  string `json:"path"`
		Grain bool   `json:"grain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Path == "" {
		http.Error(w, `expected {"path": "...", "grain": false}`, http.StatusBadRequest)
		return
	}
	s.enqueue(w, s.mapToHostPath(request.Path), request.Grain)
}

func (s *Server) enqueue(w http.ResponseWriter, hostPath string, grain bool) {
	inserted, err := s.Store.Enqueue(hostPath, grain)
	if err != nil {
		s.Log.Error("webhook enqueue failed", "path", hostPath, "error", err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.Log.Info("webhook enqueued", "path", hostPath, "new", inserted)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"path": hostPath, "queued": inserted})
}

// mapToHostPath translates a path as the dockerized *arr stack sees it into
// this host's view, using the first matching configured prefix.
func (s *Server) mapToHostPath(containerPath string) string {
	for _, mapping := range s.Config.PathMappings {
		if strings.HasPrefix(containerPath, mapping.From) {
			return mapping.To + strings.TrimPrefix(containerPath, mapping.From)
		}
	}
	return containerPath
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	summary, err := s.Store.Summarize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"jobs":        summary.CountByStatus,
		"bytes_saved": summary.BytesSaved,
	})
}
