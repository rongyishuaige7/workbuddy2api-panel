package upstream

import (
	"fmt"
	"io"
	"strings"
)

// ReadProbeResponse 读取完整短对话；不能把 role-only、空流或稍后到来的错误当作成功。
// 最多读取 1 MiB，防止错误上游无限输出；超时/取消由请求 context 约束。
func ReadProbeResponse(r io.Reader) (string, error) {
	if r == nil {
		return "", fmt.Errorf("upstream returned no response body")
	}
	limited := &io.LimitedReader{R: r, N: 1 << 20}
	resp, err := Aggregate(limited)
	if err != nil {
		return "", err
	}
	if limited.N == 0 {
		return "", fmt.Errorf("probe response exceeded 1 MiB")
	}
	choices, _ := resp["choices"].([]any)
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		for _, key := range []string{"content", "reasoning_content"} {
			if text, ok := message[key].(string); ok && strings.TrimSpace(text) != "" {
				return text, nil
			}
		}
	}
	return "", fmt.Errorf("upstream returned no assistant response")
}
