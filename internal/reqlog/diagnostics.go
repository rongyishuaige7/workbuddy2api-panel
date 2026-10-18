package reqlog

// RequestShape contains only counts, fixed enums and process-keyed fingerprints.
// It deliberately has no prompt, tool name, argument or arbitrary metadata field.
type RequestShape struct {
	Fingerprint string         `json:"fingerprint"`
	Messages    int            `json:"messages"`
	Roles       map[string]int `json:"roles,omitempty"`
	Tools       int            `json:"tools"`
	Tail        []MessageShape `json:"tail,omitempty"`
}

type MessageShape struct {
	Index       int    `json:"index"`
	Role        string `json:"role"`
	Bytes       int    `json:"bytes"`
	ToolCalls   int    `json:"tool_calls,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

type RequestComparison struct {
	FirstSuccess *RequestDifference `json:"first_success,omitempty"`
	LastSuccess  *RequestDifference `json:"last_success,omitempty"`
}

type RequestDifference struct {
	ReferenceRequestID  string `json:"reference_request_id"`
	BytesDelta          int    `json:"bytes_delta"`
	MessagesDelta       int    `json:"messages_delta"`
	CommonPrefix        int    `json:"common_prefix_messages"`
	FirstChangedMessage int    `json:"first_changed_message"` // zero-based; -1 means unchanged in the compared prefix
	ToolsChanged        bool   `json:"tools_changed"`
	SettingsChanged     bool   `json:"settings_changed"`
	Truncated           bool   `json:"comparison_truncated,omitempty"`
}

// ResponseTrace stores only allowlisted, validated response identifiers.
type ResponseTrace struct {
	OutboundBytes           int               `json:"outbound_bytes"`
	OutboundFingerprint     string            `json:"outbound_fingerprint,omitempty"`
	ContentType             string            `json:"content_type,omitempty"`
	BodyKind                string            `json:"body_kind,omitempty"`
	RequestIDs              map[string]string `json:"request_ids,omitempty"`
	RetryAfter              bool              `json:"retry_after_valid"`
	RetryAfterHeaderPresent bool              `json:"retry_after_header_present"`
}
