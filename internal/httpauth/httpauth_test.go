package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		{"前缀大小写不符（规范要求精确）", "sk-abc123", "bearer sk-abc123", false},
		{"多余空格", "sk-abc123", "Bearer  sk-abc123", false},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}

func TestPolicyAllowsModel(t *testing.T) {
	all := Policy{}
	if !all.Unrestricted() || !all.AllowsModel("cn:glm-5.2") {
		t.Error("空 Models 应视为不限模型")
	}
	restricted := Policy{Models: []string{"global:deepseek-v4.1-flash"}}
	if restricted.Unrestricted() {
		t.Error("带 Models 不应是 unrestricted")
	}
	if !restricted.AllowsModel("global:deepseek-v4.1-flash") {
		t.Error("白名单模型应放行")
	}
	if restricted.AllowsModel("cn:glm-5.2") {
		t.Error("白名单外模型必须拒绝")
	}
}

func TestResolve(t *testing.T) {
	friend := Policy{Key: "sk-friend", Name: "friend", Models: []string{"global:deepseek-v4.1-flash"}}
	cases := []struct {
		name      string
		authz     string
		ownerKey  string
		keys      []Policy
		wantOK    bool
		wantName  string
		wantModel string
	}{
		{"未启用鉴权", "", "", nil, true, "", ""},
		{"拥有者密钥", "Bearer sk-owner", "sk-owner", []Policy{friend}, true, "owner", ""},
		{"附加受限密钥", "Bearer sk-friend", "sk-owner", []Policy{friend}, true, "friend", "global:deepseek-v4.1-flash"},
		{"错误密钥", "Bearer sk-wrong", "sk-owner", []Policy{friend}, false, "", ""},
		{"只有附加密钥时缺头", "", "", []Policy{friend}, false, "", ""},
		{"只有附加密钥时命中", "Bearer sk-friend", "", []Policy{friend}, true, "friend", "global:deepseek-v4.1-flash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pol, ok := Resolve(req(c.authz), c.ownerKey, c.keys)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if pol.Name != c.wantName {
				t.Errorf("name=%q want %q", pol.Name, c.wantName)
			}
			if c.wantModel != "" && !pol.AllowsModel(c.wantModel) {
				t.Errorf("policy should allow %q", c.wantModel)
			}
		})
	}
}
