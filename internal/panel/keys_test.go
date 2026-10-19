package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeysCRUD(t *testing.T) {
	var saved []APIKeyView
	p := New(Config{
		APIKey: "panel-key",
		ListAPIKeys: func() ([]APIKeyView, error) {
			return saved, nil
		},
		SaveAPIKeys: func(k []APIKeyView) error {
			saved = append([]APIKeyView(nil), k...)
			return nil
		},
	})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer panel-key")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 未授权
	req := httptest.NewRequest(http.MethodGet, "/panel/api/keys", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code=%d", rec.Code)
	}

	// 新建
	rec = do(http.MethodPost, "/panel/api/keys", `{"name":"friend","models":["global:deepseek-v4.1-flash"],"rpm":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" || !strings.HasPrefix(created.Key, "sk-") || len(saved) != 1 {
		t.Fatalf("created=%+v saved=%d", created, len(saved))
	}

	// 列表
	rec = do(http.MethodGet, "/panel/api/keys", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 禁用
	rec = do(http.MethodPost, "/panel/api/keys/"+created.ID+"/toggle", "")
	if rec.Code != http.StatusOK || !saved[0].Disabled {
		t.Fatalf("toggle code=%d saved=%+v", rec.Code, saved[0])
	}

	// 编辑（名称/限速）
	rec = do(http.MethodPost, "/panel/api/keys", `{"id":"`+created.ID+`","name":"renamed","rpm":0}`)
	if rec.Code != http.StatusOK || saved[0].Name != "renamed" || saved[0].RPM != 0 {
		t.Fatalf("update code=%d saved=%+v", rec.Code, saved[0])
	}

	// 轮换：ID 不变、key 变
	rec = do(http.MethodPost, "/panel/api/keys/"+created.ID+"/rotate", "")
	if rec.Code != http.StatusOK || saved[0].ID != created.ID || saved[0].Key == created.Key {
		t.Fatalf("rotate code=%d saved=%+v", rec.Code, saved[0])
	}

	// 删除
	rec = do(http.MethodPost, "/panel/api/keys/"+created.ID+"/remove", "")
	if rec.Code != http.StatusOK || len(saved) != 0 {
		t.Fatalf("remove code=%d saved=%d", rec.Code, len(saved))
	}
}

func TestNewAPIKeyAndCleanModels(t *testing.T) {
	k, err := newAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k, "sk-") || len(k) != 35 {
		t.Fatalf("key=%q len=%d", k, len(k))
	}
	if keyID(k) != keyID(k) || keyID(k) == keyID(k+"x") {
		t.Fatal("keyID should be stable and key-dependent")
	}
	got := cleanModels([]string{" a ", "", "a", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("cleanModels=%v", got)
	}
}
