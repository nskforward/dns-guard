package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nskforward/dns-guard/internal/stats"
)

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
	if info.Last == nil || info.Last.Rcode != "NOERROR" || info.Last.Upstream != "1.1.1.1:53" {
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
