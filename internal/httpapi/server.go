package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/LJAYi/komari-bridge/internal/slurm"
)

type Server struct {
	slurm  *slurm.Store
	apiKey string
}

func New(slurmStore *slurm.Store, apiKey string) *Server {
	return &Server{slurm: slurmStore, apiKey: apiKey}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /api/v1/slurm", s.authorize(http.HandlerFunc(s.allSlurm)))
	mux.Handle("GET /api/v1/slurm/{source}", s.authorize(http.HandlerFunc(s.oneSlurm)))
	return mux
}

func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(s.apiKey)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allSlurm(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.slurm.All())
}

func (s *Server) oneSlurm(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := s.slurm.Get(r.PathValue("source"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "source not found"})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
