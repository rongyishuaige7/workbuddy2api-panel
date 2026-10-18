package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponseTraceWAFIDsAndPrivacy(t *testing.T) {
	const id = "a6774c8b46f486372b0107d84c19b15a"
	headers := http.Header{}
	headers.Set("Content-Type", "text/html; charset=utf-8")
	headers.Set("X-Request-ID", id)
	headers.Set("CF-Ray", "0123456789abcdef-SJC")
	headers.Set("Retry-After", "30")
	headers.Set("Set-Cookie", "private-cookie")
	headers.Set("Authorization", "Bearer private-token")
	headers.Set("X-Trace-ID", "Bearer private-token")
	headers.Set("X-Untrusted-Header", "private-value")
	body := []byte(`<html><title>WAF Block Page</title><body>` + strings.Repeat("private-body ", 100) + `Request ID: <b>` + id + `</b></body></html>`)
	trace := responseTrace(headers, body)
	if trace.ContentType != "text/html" || trace.BodyKind != "waf_block_page" || !trace.RetryAfter || !trace.RetryAfterHeaderPresent || trace.RequestIDs["x-request-id"] != id || trace.RequestIDs["body-request-id"] != id || trace.RequestIDs["cf-ray"] == "" || len(trace.RequestIDs) != 3 {
		t.Fatalf("unexpected WAF trace: %+v", trace)
	}
	raw, _ := json.Marshal(trace)
	for _, secret := range []string{"private-cookie", "private-token", "private-value", "private-body", "WAF Block Page"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("trace contains private data %q", secret)
		}
	}
	for _, bad := range []string{"Bearer secret", strings.Repeat("a", 65), "0123456789abcdef-extra-secret", "<script>private</script>"} {
		headers.Set("X-Request-ID", bad)
		if responseTrace(headers, nil).RequestIDs["x-request-id"] != "" {
			t.Fatalf("unvalidated trace ID escaped: %q", bad)
		}
	}
	headers.Set("Retry-After", "unparseable")
	trace = responseTrace(headers, nil)
	if !trace.RetryAfterHeaderPresent || trace.RetryAfter {
		t.Fatal("must distinguish absent retry headers from present but unparsed headers")
	}
}
