package upstream

import (
	"mime"
	"net/http"
	"regexp"
	"strings"
)

type ResponseTrace struct {
	ContentType             string
	BodyKind                string
	RequestIDs              map[string]string
	RetryAfter              bool
	RetryAfterHeaderPresent bool
}

var (
	wafTitle      = regexp.MustCompile(`(?i)<title>\s*WAF\s+Block\s+Page\s*</title>`)
	pageRequestID = regexp.MustCompile(`(?i)\b(?:request|trace)[_ -]?id\s*(?:</[^>]{1,32}>\s*)?[:=]\s*(?:<[^>]{1,32}>\s*)?([0-9a-f][0-9a-f-]{15,63})(?:[^0-9a-z_-]|$)`)
)

// responseTrace never copies arbitrary headers or page text into diagnostics.
func responseTrace(headers http.Header, body []byte) ResponseTrace {
	trace := ResponseTrace{}
	contentType, _, _ := mime.ParseMediaType(headers.Get("Content-Type"))
	switch strings.ToLower(contentType) {
	case "application/json", "text/html", "text/plain", "text/event-stream":
		trace.ContentType = strings.ToLower(contentType)
	case "":
	default:
		trace.ContentType = "other"
	}
	for _, name := range []string{"X-Request-Id", "X-Trace-Id", "X-APISIX-Request-Id", "X-WAF-Request-Id", "X-TC-RequestId", "X-Tencent-Request-Id", "CF-Ray"} {
		id := headers.Get(name)
		valid := validResponseID(id)
		if name == "CF-Ray" {
			valid = validRayID(id)
		}
		if valid {
			if trace.RequestIDs == nil {
				trace.RequestIDs = make(map[string]string)
			}
			trace.RequestIDs[strings.ToLower(name)] = id
		}
	}
	if len(body) > 0 {
		switch {
		case wafTitle.Match(body):
			trace.BodyKind = "waf_block_page"
		case trace.ContentType == "text/html":
			trace.BodyKind = "html"
		case trace.ContentType == "application/json":
			trace.BodyKind = "json"
		default:
			trace.BodyKind = "other"
		}
		if match := pageRequestID.FindSubmatch(body); len(match) > 1 && validResponseID(string(match[1])) {
			if trace.RequestIDs == nil {
				trace.RequestIDs = make(map[string]string)
			}
			trace.RequestIDs["body-request-id"] = string(match[1])
		}
	}
	_, trace.RetryAfter = ParseRetryAfter(headers)
	for _, name := range retryAfterHeaderCandidates {
		if strings.TrimSpace(headers.Get(name)) != "" {
			trace.RetryAfterHeaderPresent = true
			break
		}
	}
	return trace
}

func validResponseID(value string) bool {
	if len(value) < 16 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == '-') {
			return false
		}
	}
	return true
}

func validRayID(value string) bool {
	if len(value) != 20 || value[16] != '-' || !validResponseID(value[:16]) {
		return false
	}
	for _, c := range value[17:] {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}
