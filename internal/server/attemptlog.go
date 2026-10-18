package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const maxAttemptDetails = 16

func sessionHash(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func (s *chatStat) recordAttemptDetail(a reqlog.Attempt, started time.Time, durationMs int64) {
	a.StartedAt, a.DurationMs = started, durationMs
	if len(s.attemptDetails) < maxAttemptDetails {
		s.attemptDetails = append(s.attemptDetails, a)
	} else {
		// 留住前几跳与最终尝试；自定义 MaxRotate 再大也不能令单条日志无界增长。
		s.attemptDetails[maxAttemptDetails-1] = a
	}
}

func noteAttemptError(a *reqlog.Attempt, err error, body string) {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue != nil {
		a.ErrorKind = ue.Kind.String()
		a.RetryAfterMs = ue.RetryAfter.Milliseconds()
		if body == "" {
			body = ue.Msg
		}
	} else if err != nil {
		a.ErrorKind = "transport_error"
	} else if a.UpstreamStatus >= 400 {
		a.ErrorKind = upstream.Classify(a.UpstreamStatus, body).String()
	}
	a.BusinessCode, a.UpstreamRequestID = errorDiagnosticIDs(body)
}

// 仅允许数值业务码与 hex/UUID 请求 ID。嵌套 message JSON 最多展开四层；
// 其他字段（包括原始错误文案）一律不进入请求归档。
func errorDiagnosticIDs(body string) (code, requestID string) {
	for depth := 0; depth < 4 && len(body) <= 1<<20; depth++ {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(body), &obj) != nil {
			break
		}
		if raw := obj["code"]; len(raw) > 0 {
			v := strings.Trim(string(raw), `"`)
			if v != "0" && validDiagnosticID(v, 1, 16, false) {
				code = v
			}
		}
		var id string
		if json.Unmarshal(obj["requestId"], &id) == nil && validDiagnosticID(id, 16, 64, true) {
			requestID = id
		}
		if inner := strings.TrimSpace(string(obj["error"])); strings.HasPrefix(inner, "{") {
			body = inner
			continue
		}
		var nested string
		for _, key := range []string{"message", "msg"} {
			var msg string
			if json.Unmarshal(obj[key], &msg) == nil && strings.HasPrefix(strings.TrimSpace(msg), "{") {
				nested = msg
				break
			}
		}
		if nested == "" {
			break
		}
		body = nested
	}
	return code, requestID
}

func validDiagnosticID(v string, minLen, maxLen int, hexID bool) bool {
	if len(v) < minLen || len(v) > maxLen {
		return false
	}
	for _, c := range v {
		if c >= '0' && c <= '9' {
			continue
		}
		if hexID && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == '-') {
			continue
		}
		return false
	}
	return true
}
