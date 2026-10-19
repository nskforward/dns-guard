// Package web предоставляет встроенный веб-интерфейс и REST API для
// мониторинга статистики и редактирования конфигурации.
package web

import (
	"embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/rules"
	"github.com/nskforward/dns-guard/internal/stats"
)

//go:embed index.html
var assets embed.FS

// Deps — зависимости веб-сервера.
type Deps struct {
	Path    string                         // путь к файлу конфигурации
	Version string                         // версия приложения для UI и API
	Stats   *stats.Stats                   // сборщик статистики
	Cache   *cache.Cache                   // кэш (может быть nil)
	Config  func() *config.Config          // текущая конфигурация
	Reload  func(cfg *config.Config) error // применение конфигурации на лету
	Log     *slog.Logger
}

// Server — HTTP-сервер веб-интерфейса.
type Server struct {
	deps Deps
	mux  *http.ServeMux
	// mu сериализует изменения конфигурации (блоклист и PUT /api/config):
	// оба обработчика делают read-modify-write одного и того же файла.
	mu sync.Mutex
}

// New создаёт HTTP-обработчик веб-интерфейса.
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/version", s.handleVersion)
	s.mux.HandleFunc("/api/stats", s.handleStats)
	s.mux.HandleFunc("/api/domains", s.handleDomains)
	s.mux.HandleFunc("/api/domain", s.handleDomain)
	s.mux.HandleFunc("/api/upstreams", s.handleUpstreams)
	s.mux.HandleFunc("/api/block", s.handleBlock)
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
	// UI меняется вместе с бинарником: запрещаем кэшировать HTML, иначе после
	// обновления контейнера вкладка продолжает работать со старым JS.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	v := s.deps.Version
	if v == "" {
		v = "dev"
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": v})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	resp := statsResponse{}
	if s.deps.Stats != nil {
		resp.Snapshot = s.deps.Stats.Snapshot()
	}
	if s.deps.Cache != nil {
		resp.CacheLen = s.deps.Cache.Len()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDomains отдаёт страницу списка доменов:
// GET /api/domains?page=1&limit=50&search=строка&blocked=1&bad=1&status=bad
// blocked=1 оставляет только заблокированные домены; bad=1 — только домены с
// проблемным последним ответом (bad/servfail/nxdomain/nodata); status фильтрует
// по точному статусу последнего ответа (ok/servfail/nxdomain/nodata/blocked/bad).
func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	size := queryInt(r, "limit", stats.DefaultPageSize)
	search := r.URL.Query().Get("search")
	blockedOnly := queryBool(r, "blocked")
	badOnly := queryBool(r, "bad")
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	var isBlocked func(string) (string, bool)
	if bl := s.blocklist(); bl.Len() > 0 {
		isBlocked = bl.Match
	}
	if s.deps.Stats == nil {
		if size <= 0 {
			size = stats.DefaultPageSize
		}
		if size > stats.MaxPageSize {
			size = stats.MaxPageSize
		}
		if page < 1 {
			page = 1
		}
		writeJSON(w, http.StatusOK, stats.DomainsPage{Page: page, Size: size, Items: []stats.DomainStat{}})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Stats.DomainsPage(page, size, search, blockedOnly, badOnly, status, isBlocked))
}

// handleDomain отдаёт статистику домена и сведения о последнем ответе:
// GET /api/domain?name=example.com&type=A
// Необязательный type ограничивает ответ одной парой (домен, тип): в ответе
// остаётся только последний ответ этого типа и его счётчики.
func (s *Server) handleDomain(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if s.deps.Stats == nil {
		http.Error(w, "stats unavailable", http.StatusNotFound)
		return
	}
	info, ok := s.deps.Stats.DomainInfoType(name, r.URL.Query().Get("type"))
	if !ok {
		http.Error(w, "domain not found", http.StatusNotFound)
		return
	}
	if entry, blocked := s.blocklist().Match(name); blocked {
		info.Blocked = true
		info.BlockedBy = entry
	}
	writeJSON(w, http.StatusOK, info)
}

// handleBlock добавляет домен в блоклист (PUT) или убирает его (DELETE):
// PUT|DELETE /api/block?name=example.com
// Изменение применяется на лету и сохраняется в файл конфигурации.
func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := config.NormalizeDomain(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.currentConfig()
	if cfg == nil {
		http.Error(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	next := cfg.Clone()
	blocked := r.Method == http.MethodPut
	if blocked {
		next.Block.Domains = append(next.Block.Domains, name)
	} else {
		next.Block.Domains = removeDomain(next.Block.Domains, name)
	}
	next.Normalize()
	if err := next.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps.Reload != nil {
		if err := s.deps.Reload(next); err != nil {
			http.Error(w, "apply config: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if s.deps.Path != "" {
		if err := next.Save(s.deps.Path); err != nil {
			s.deps.Log.Error("save config", "path", s.deps.Path, "err", err)
			http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.deps.Log.Info("blocklist updated via web", "domain", name, "blocked", blocked)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"domain":  name,
		"blocked": blocked,
		"domains": next.Block.Domains,
	})
}

// removeDomain удаляет домен из списка, сохраняя порядок остальных.
func removeDomain(domains []string, name string) []string {
	out := domains[:0]
	for _, d := range domains {
		if d != name {
			out = append(out, d)
		}
	}
	return out
}

// blocklist собирает матчер блоклиста по текущей конфигурации.
func (s *Server) blocklist() *rules.Blocklist {
	cfg := s.currentConfig()
	if cfg == nil {
		return rules.NewBlocklist(nil)
	}
	return rules.NewBlocklist(cfg.Block.Domains)
}

// handleUpstreams отдаёт статистику по группам апстримов:
// GET /api/upstreams
func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	items := []stats.GroupStat{}
	if s.deps.Stats != nil {
		items = s.deps.Stats.UpstreamsPage()
	}
	writeJSON(w, http.StatusOK, map[string][]stats.GroupStat{"items": items})
}

// queryInt разбирает целочисленный query-параметр, возвращая def при ошибке.
func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// queryBool разбирает булев query-параметр (1/true/yes/on).
func queryBool(r *http.Request, key string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
