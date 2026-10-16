// Package web предоставляет встроенный веб-интерфейс и REST API для
// мониторинга статистики и редактирования конфигурации.
package web

import (
	"embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/stats"
)

//go:embed index.html
var assets embed.FS

const topN = 100

// Deps — зависимости веб-сервера.
type Deps struct {
	Path   string                         // путь к файлу конфигурации
	Stats  *stats.Stats                   // сборщик статистики
	Cache  *cache.Cache                   // кэш (может быть nil)
	Config func() *config.Config          // текущая конфигурация
	Reload func(cfg *config.Config) error // применение конфигурации на лету
	Log    *slog.Logger
}

// Server — HTTP-сервер веб-интерфейса.
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// New создаёт HTTP-обработчик веб-интерфейса.
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/stats", s.handleStats)
	s.mux.HandleFunc("/api/config", s.handleConfig)
	s.mux.HandleFunc("/api/cache/flush", s.handleFlush)
	return s
}

// Handler возвращает HTTP-обработчик.
func (s *Server) Handler() http.Handler { return s.mux }

type statsResponse struct {
	stats.Snapshot
	CacheLen int `json:"cache_len"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	resp := statsResponse{}
	if s.deps.Stats != nil {
		resp.Snapshot = s.deps.Stats.Snapshot(topN)
	}
	if s.deps.Cache != nil {
		resp.CacheLen = s.deps.Cache.Len()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		if cfg == nil {
			http.Error(w, "config unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"yaml": cfg.YAML()})
	case http.MethodPut:
		s.putConfig(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := config.Parse(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps.Reload != nil {
		if err := s.deps.Reload(cfg); err != nil {
			http.Error(w, "apply config: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if s.deps.Path != "" {
		if err := cfg.Save(s.deps.Path); err != nil {
			s.deps.Log.Error("save config", "path", s.deps.Path, "err", err)
			http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.deps.Log.Info("config updated via web", "path", s.deps.Path)
	writeJSON(w, http.StatusOK, map[string]string{"yaml": cfg.YAML()})
}

func (s *Server) handleFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Cache != nil {
		s.deps.Cache.Flush()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) currentConfig() *config.Config {
	if s.deps.Config == nil {
		return nil
	}
	return s.deps.Config()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
