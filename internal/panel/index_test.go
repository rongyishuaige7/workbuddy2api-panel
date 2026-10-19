package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticETagAndCacheScope(t *testing.T) {
	p := New(Config{APIKey: "k"})

	req := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static page should carry ETag")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("static cache-control=%q", cc)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("revalidate code=%d", rec2.Code)
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/panel/api/keys", nil)
	apiReq.Header.Set("Authorization", "Bearer k")
	apiRec := httptest.NewRecorder()
	p.ServeHTTP(apiRec, apiReq)
	if cc := apiRec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("api cache-control=%q", cc)
	}
}
