package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// TestPanelAPIKeyPrecedence 锁定面板密钥的回落链：
// Live.PanelAPIKey → Live.APIKey → 静态 PanelAPIKey → 静态 APIKey。
func TestPanelAPIKeyPrecedence(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"只有网关密钥", Config{APIKey: "api"}, "api"},
		{"静态面板密钥优先", Config{APIKey: "api", PanelAPIKey: "panel"}, "panel"},
		{"Live 面板密钥优先", Config{
			APIKey: "api", PanelAPIKey: "panel",
			Live: livecfg.New(livecfg.Snapshot{APIKey: "api", PanelAPIKey: "panel-live"}),
		}, "panel-live"},
		{"Live 面板密钥为空回落 Live 网关密钥", Config{
			APIKey: "api", PanelAPIKey: "panel",
			Live: livecfg.New(livecfg.Snapshot{APIKey: "api-live"}),
		}, "api-live"},
		{"全空 = 不鉴权", Config{}, ""},
	}
	for _, tc := range cases {
		if got := New(tc.cfg).apiKey(); got != tc.want {
			t.Errorf("%s: apiKey()=%q want %q", tc.name, got, tc.want)
		}
	}
}

// TestPanelWithAuthUsesPanelKey 验证面板接口只认面板密钥：
// 网关密钥即使正确也必须被拒，避免「给他人 api_key = 给管理权」。
func TestPanelWithAuthUsesPanelKey(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "api-key", PanelAPIKey: "panel-key"})
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	handler := p.withAuth(ok)

	call := func(bearer string) int {
		req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}

	if got := call("panel-key"); got != http.StatusOK {
		t.Fatalf("面板密钥应放行，got %d", got)
	}
	if got := call("api-key"); got != http.StatusUnauthorized {
		t.Fatalf("网关密钥不应获得面板权限，got %d", got)
	}
	if got := call(""); got != http.StatusUnauthorized {
		t.Fatalf("无密钥应 401，got %d", got)
	}
}
