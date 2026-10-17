package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestServfailAPI(t *testing.T) {
	st := stats.New(0)
	st.Record("a.example.com.", "g1", false, &stats.AnswerInfo{
		Rcode:    "SERVFAIL",
		Qtype:    "A",
		Error:    "timeout",
		Upstream: "1.1.1.1:53",
	})
	st.Record("a.example.com.", "g1", true, &stats.AnswerInfo{
		Rcode:     "SERVFAIL",
		Qtype:     "AAAA",
		FromCache: true,
	})
	st.Record("b.example.com.", "g2", false, &stats.AnswerInfo{
		Rcode: "NOERROR",
		Qtype: "A",
	})

	h := New(Deps{Stats: st}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/servfail?page=1&limit=10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page stats.ServfailPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 2 || page.DomainsTotal != 1 || len(page.Domains) != 1 {
		t.Fatalf("page = %+v", page)
	}
	d := page.Domains[0]
	if d.Domain != "a.example.com" || d.Count != 2 || d.Last == nil || !d.Last.FromCache {
		t.Fatalf("domain = %+v", d)
	}
	if len(page.Recent) != 2 || page.Recent[0].Group != "g1" || !page.Recent[0].FromCache {
		t.Fatalf("recent = %+v", page.Recent)
	}
	if page.Recent[1].Error != "timeout" || page.Recent[1].Upstream != "1.1.1.1:53" {
		t.Fatalf("recent[1] = %+v", page.Recent[1])
	}

	// Без сборщика статистики — пустой корректный ответ.
	rec = httptest.NewRecorder()
	New(Deps{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/servfail", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("nil stats status = %d", rec.Code)
	}
	var empty stats.ServfailPage
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if empty.Page != 1 || empty.Size != stats.DefaultPageSize || len(empty.Domains) != 0 {
		t.Fatalf("empty = %+v", empty)
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
