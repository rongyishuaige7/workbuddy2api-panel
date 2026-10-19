package main

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/promptlog"
)

func TestPromptManagementSurvivesCaptureDisabled(t *testing.T) {
	cfg := &Config{StateFile: filepath.Join(t.TempDir(), "state.json"), APIKey: "admin"}
	dir := stateSibling(cfg.StateFile, "prompt-logs")
	old := promptlog.New(promptlog.Config{Dir: dir})
	if err := old.Record(promptlog.Entry{KeyID: "k"}); err != nil {
		t.Fatal(err)
	}
	old.Close()
	manager, capture, err := setupPromptLogs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if capture != nil || manager == nil {
		t.Fatal("capture and management must have independent lifecycles")
	}
	if entries, err := manager.List("k", 10); err != nil || len(entries) != 1 {
		t.Fatal("old archive unavailable after capture disabled")
	}
	if n, err := manager.Clear("k"); err != nil || n != 1 {
		t.Fatal("cannot clear when capture disabled")
	}
	cfg.Logging.PromptCaptureEnabled = true
	cfg.APIKey = ""
	if _, _, err := setupPromptLogs(cfg); err == nil {
		t.Fatal("capture without administrative auth must fail closed")
	}
	cfg.Panel.APIKey = "panel"
	other, enabled, err := setupPromptLogs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if enabled != other {
		t.Fatal("capture not wired when enabled")
	}
}
