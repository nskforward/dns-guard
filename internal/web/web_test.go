package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/stats"
)

func TestVersionAPI(t *testing.T) {
	h := New(Deps{Version: "1.2.3"}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["version"] != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", body["version"])
	}

	// Пустая версия означает локальную сборку.
	rec = httptest.NewRecorder()
	New(Deps{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode default: %v", err)
	}
	if body["version"] != "dev" {
		t.Fatalf("default version = %q, want dev", body["version"])
	}
}

func TestDomainsAPI(t *testing.T) {
	st := stats.New(0)
	st.Record("a.example.com.", "g", false, &stats.AnswerInfo{
		Rcode:    "NOERROR",
		Qtype:    "A",
		Records:  []string{"a.example.com. 60 IN A 1.2.3.4"},
		Upstream: "1.1.1.1:53",
	})
	st.Record("b.other.net.", "g", false, nil)
	st.Record("c.example.com.", "g", true, &stats.AnswerInfo{
		Rcode:     "NXDOMAIN",
		Qtype:     "AAAA",
		FromCache: true,
	})
	h := New(Deps{Stats: st}).Handler()

	// Список с поиском и размером страницы.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domains?page=1&limit=1&search=example", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page stats.DomainsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 2 || page.Pages != 2 || page.Page != 1 || len(page.Items) != 1 {
		t.Fatalf("page = %+v", page)
	}
	if page.Items[0].Status != "ok" {
		t.Fatalf("status = %q, want ok", page.Items[0].Status)
	}

	// bad=1 оставляет только домены с проблемным последним ответом.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domains?bad=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var badPage stats.DomainsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &badPage); err != nil {
		t.Fatalf("decode bad: %v", err)
	}
	if badPage.Total != 1 || len(badPage.Items) != 1 || badPage.Items[0].Domain != "c.example.com" {
		t.Fatalf("bad page = %+v", badPage)
	}

	// Детали домена с последним ответом.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domain?name=a.example.com", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var info stats.DomainInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Domain != "a.example.com" || info.Total != 1 {
		t.Fatalf("info = %+v", info)
	}
	if info.Answer == nil || info.Answer.Rcode != "NOERROR" || info.Answer.Upstream != "1.1.1.1:53" {
		t.Fatalf("answer = %+v", info.Answer)
	}
	if info.Last == nil || info.Last.Rcode != "NOERROR" || info.Last.Qtype != "A" ||
		info.Last.Upstream != "1.1.1.1:53" {
		t.Fatalf("last = %+v", info.Last)
	}

	// Неизвестный домен.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domain?name=missing.test", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing domain status = %d, want 404", rec.Code)
	}

	// Пустое имя.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domain", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, want 400", rec.Code)
	}
}

func TestDomainNegativeAPI(t *testing.T) {
	st := stats.New(0)
	st.Record("nx.example.com.", "g", false, &stats.AnswerInfo{
		Rcode: "NOERROR",
		Qtype: "AAAA",
		Attempts: []stats.AttemptInfo{
			{Upstream: "udp://192.168.1.1:53", Status: "NODATA", DurationMs: 4},
			{Upstream: "doh+https://9.9.9.9/dns-query", Status: "NODATA", DurationMs: 40},
		},
	})
	h := New(Deps{Stats: st}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domain?name=nx.example.com", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var info stats.DomainInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Answer != nil {
		t.Fatalf("answer = %+v, want nil", info.Answer)
	}
	if len(info.Negative) != 1 {
		t.Fatalf("negative = %+v", info.Negative)
	}
	if info.Last == nil || info.Last.Rcode != "NOERROR" || info.Last.Qtype != "AAAA" {
		t.Fatalf("last = %+v", info.Last)
	}
	neg := info.Negative[0]
	if neg.Qtype != "AAAA" || neg.Rcode != "NOERROR" || len(neg.Attempts) != 2 {
		t.Fatalf("negative = %+v", neg)
	}
	if neg.Attempts[1].Status != "NODATA" || neg.Attempts[1].DurationMs != 40 {
		t.Fatalf("attempts = %+v", neg.Attempts)
	}
}

func TestUpstreamsAPI(t *testing.T) {
	st := stats.New(0)
	st.Record("a.example.com.", "g1", false, nil)
	st.Record("b.example.com.", "g1", true, nil)
	st.Record("c.example.com.", "g2", false, nil)
	st.RecordUpstream("g1", "1.1.1.1:53", false, 10*time.Millisecond)
	st.RecordUpstream("g1", "8.8.8.8:53", true, 100*time.Millisecond)
	st.RecordUpstream("g2", "9.9.9.9:53", false, 5*time.Millisecond)

	h := New(Deps{Stats: st}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/upstreams", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []stats.GroupStat `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %+v", body.Items)
	}
	if body.Items[0].Name != "g1" || body.Items[0].Total != 2 || body.Items[0].CacheHits != 1 ||
		body.Items[0].Failed != 1 || len(body.Items[0].Upstreams) != 2 {
		t.Fatalf("g1 = %+v", body.Items[0])
	}
	for _, u := range body.Items[0].Upstreams {
		if u.Upstream == "1.1.1.1:53" && u.P90Ms != 10 {
			t.Fatalf("1.1.1.1 p90 = %v, want 10", u.P90Ms)
		}
	}
}

func TestStatsAPIWithoutTop(t *testing.T) {
	st := stats.New(0)
	st.Record("a.example.com.", "g", false, nil)
	h := New(Deps{Stats: st}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["top"]; ok {
		t.Fatal("/api/stats must not contain top anymore")
	}
	if body["total"].(float64) != 1 {
		t.Fatalf("total = %v", body["total"])
	}
}

// TestBlockAPI проверяет блокировку/разблокировку домена через API: применение
// конфига, сохранение в файл, флаги в списке доменов и фильтр blocked=1.
func TestBlockAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	st := stats.New(0)
	st.Record("sub.ads.example.com.", "g", false, nil)
	st.Record("other.net.", "g", false, nil)

	var applied *config.Config
	h := New(Deps{
		Path:   path,
		Stats:  st,
		Config: func() *config.Config { return cfg },
		Reload: func(c *config.Config) error { applied = c; cfg = c; return nil },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()

	// PUT блокирует домен (имя нормализуется) и сохраняет конфиг.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/block?name=ADS.Example.COM.", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if applied == nil || len(applied.Block.Domains) != 1 || applied.Block.Domains[0] != "ads.example.com" {
		t.Fatalf("applied blocklist = %+v", applied)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(data), "ads.example.com") {
		t.Fatalf("saved config has no block entry:\n%s", data)
	}

	// Список доменов: флаг блокировки ставится и для поддомена, фильтр оставляет
	// только заблокированный.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domains?blocked=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("domains status = %d", rec.Code)
	}
	var page stats.DomainsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("filtered page = %+v", page)
	}
	if item := page.Items[0]; item.Domain != "sub.ads.example.com" || !item.Blocked || item.BlockedBy != "ads.example.com" {
		t.Fatalf("item = %+v", item)
	}

	// Без фильтра оба домена, у незаблокированного blocked=false.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domains", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode all: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("total = %d, want 2", page.Total)
	}

	// Детали домена содержат блокировку и её источник.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domain?name=sub.ads.example.com", nil))
	var info stats.DomainInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode domain: %v", err)
	}
	if !info.Blocked || info.BlockedBy != "ads.example.com" {
		t.Fatalf("domain info = %+v", info)
	}

	// DELETE снимает блокировку (повторный DELETE идемпотентен).
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/block?name=ads.example.com", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	if applied == nil || len(applied.Block.Domains) != 0 {
		t.Fatalf("blocklist after delete = %+v", applied)
	}

	// Ошибки: неподдерживаемый метод и пустое имя.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/block?name=example.com", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/block", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, want 400", rec.Code)
	}
}

// TestDomainsStatusFilterAPI проверяет фильтр status=bad в /api/domains:
// в ответ попадают только домены с плохим последним ответом.
func TestDomainsStatusFilterAPI(t *testing.T) {
	st := stats.New(0)
	st.Record("ok.test.", "g", false, &stats.AnswerInfo{
		Rcode: "NOERROR", Qtype: "A", Records: []string{"ok.test. 60 IN A 1.2.3.4"},
	})
	st.Record("bad.test.", "g", false, &stats.AnswerInfo{Rcode: "REFUSED", Qtype: "A"})
	h := New(Deps{
		Stats:  st,
		Config: func() *config.Config { return config.Default() },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domains?status=bad", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var page stats.DomainsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Domain != "bad.test" {
		t.Fatalf("filtered page = %+v", page)
	}

	// Неизвестный статус не должен ничего находить.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/domains?status=nope", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("unknown status page = %+v", page)
	}
}
