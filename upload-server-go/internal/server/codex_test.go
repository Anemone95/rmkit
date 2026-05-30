package server

import "testing"

func TestNormalizeCodexReasoningEffort(t *testing.T) {
	tests := map[string]string{
		"":            "low",
		"low":         "low",
		"medium":      "medium",
		"high":        "high",
		"xhigh":       "xhigh",
		"extra high":  "xhigh",
		"extra-high":  "xhigh",
		"extra_high":  "xhigh",
		" EXTRA HIGH": "xhigh",
	}
	for in, want := range tests {
		got, ok := normalizeCodexReasoningEffort(in)
		if !ok {
			t.Fatalf("normalizeCodexReasoningEffort(%q) returned !ok", in)
		}
		if got != want {
			t.Fatalf("normalizeCodexReasoningEffort(%q)=%q want %q", in, got, want)
		}
	}
	if got, ok := normalizeCodexReasoningEffort("max"); ok || got != "" {
		t.Fatalf("normalizeCodexReasoningEffort(max)=%q,%v want empty,false", got, ok)
	}
}

func TestNormalizeAIConfigCodexDefaultsReasoning(t *testing.T) {
	cfg := normalizeAIConfig(aiConfig{Kind: "codex"})
	if cfg.ReasoningEffort != codexDefaultReasoningEffort {
		t.Fatalf("ReasoningEffort=%q want %q", cfg.ReasoningEffort, codexDefaultReasoningEffort)
	}
}
