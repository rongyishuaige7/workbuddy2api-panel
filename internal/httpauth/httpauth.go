// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// Policy 一把下游 API 密钥的权限视图。
//
// 顶层 api_key 是拥有者密钥（Models 为空 = 不限模型）；api_keys 里的附加密钥
// 可各带模型白名单，用于「把 key 给别人但只允许用某个模型」。
type Policy struct {
	ID       string   // 稳定标识（统计/限速按它聚合；轮换密钥值不变）
	Key      string   // 密钥原文（仅内存；不写日志）
	Name     string   // 标识（日志/排障用，可为空）
	Models   []string // 允许的模型 id；空 = 不限
	RPM      int      // 每分钟请求上限；0 = 不限
	Disabled bool     // 已禁用：鉴权命中但拒绝服务（403 key_disabled）
}

// Unrestricted 是否不限制模型。
func (p Policy) Unrestricted() bool { return len(p.Models) == 0 }

// AllowsModel 判断模型是否在允许集合内；不限制时恒真。
func (p Policy) AllowsModel(model string) bool {
	if len(p.Models) == 0 {
		return true
	}
	for _, m := range p.Models {
		if m == model {
			return true
		}
	}
	return false
}

// Resolve 校验请求 Bearer 并返回命中的密钥策略。
//
// ownerKey 非空时作为不限制模型的拥有者密钥；keys 为附加密钥（可带模型白名单）。
// 两者都为空表示未启用鉴权：返回零值 Policy 与 true。
// 比较同样走 SHA-256 摘要 + 常量时间比较，且不因先命中而提前返回（减少时序差异）。
func Resolve(r *http.Request, ownerKey string, keys []Policy) (Policy, bool) {
	if ownerKey == "" && len(keys) == 0 {
		return Policy{}, true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		subtle.ConstantTimeCompare(digest(""), digest(ownerKey))
		return Policy{}, false
	}
	tok := authz[len(bearerPrefix):]
	var hit Policy
	found := false
	if ownerKey != "" && subtle.ConstantTimeCompare(digest(tok), digest(ownerKey)) == 1 {
		hit, found = Policy{ID: "owner", Name: "owner"}, true
	}
	for _, k := range keys {
		if k.Key == "" {
			continue
		}
		if subtle.ConstantTimeCompare(digest(tok), digest(k.Key)) == 1 {
			hit, found = k, true
		}
	}
	return hit, found
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
