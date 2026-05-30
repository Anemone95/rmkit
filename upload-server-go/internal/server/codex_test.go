package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

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

func TestCodexInitialDocumentPrompt(t *testing.T) {
	got := codexInitialDocumentPrompt(context.Background(), "translate this", "/home/root/.local/share/remarkable/xochitl/doc-1.pdf", false)
	want := "@/home/root/.local/share/remarkable/xochitl/doc-1.pdf\n\ntranslate this"
	if got != want {
		t.Fatalf("prompt=%q want %q", got, want)
	}
	if got := codexInitialDocumentPrompt(context.Background(), "translate this", "/tmp/doc.pdf", false); got != "translate this" {
		t.Fatalf("unsafe path should be ignored, got %q", got)
	}
	if got := normalizeCodexDocumentPathForDocument("doc-1", "/home/root/.local/share/remarkable/xochitl/doc-2.pdf"); got != "" {
		t.Fatalf("mismatched document path=%q want empty", got)
	}
}

func TestCodexInitialDocumentPromptIncludesDocumentText(t *testing.T) {
	old := codexDocumentTextExtractor
	t.Cleanup(func() { codexDocumentTextExtractor = old })
	codexDocumentTextExtractor = func(context.Context, string) (codexDocumentText, error) {
		return codexDocumentText{Text: "Paper title\nImportant result.", Truncated: false}, nil
	}

	got := codexInitialDocumentPrompt(context.Background(),
		"请你跟我总结这个论文内容",
		"/home/root/.local/share/remarkable/xochitl/doc-1.pdf",
		true)
	if !strings.Contains(got, "@/home/root/.local/share/remarkable/xochitl/doc-1.pdf") {
		t.Fatalf("prompt missing document path: %q", got)
	}
	if !strings.Contains(got, "<document_text path=\"/home/root/.local/share/remarkable/xochitl/doc-1.pdf\">") ||
		!strings.Contains(got, "Paper title\nImportant result.") ||
		!strings.HasSuffix(got, "请你跟我总结这个论文内容") {
		t.Fatalf("prompt missing extracted text: %q", got)
	}
}

func TestShouldAttachDocumentText(t *testing.T) {
	if !shouldAttachDocumentText("请你跟我总结这个论文内容") {
		t.Fatal("summary prompt should attach document text")
	}
	if shouldAttachDocumentText("请帮我翻译这句话为中文:hello") {
		t.Fatal("selection translation should not attach whole document text")
	}
}

func TestAIPageChatReusesDocumentThread(t *testing.T) {
	_, h, root := newTestServer(t)
	wsURL, connections := newFakeCodexAppServer(t)
	cfg := aiConfig{
		Kind:            "codex",
		URL:             wsURL,
		Model:           "gpt-test",
		ReasoningEffort: "low",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ai_config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	first := postAIPageChat(t, h, `{"prompt":"first prompt","document_id":"doc-1","document_path":"/home/root/.local/share/remarkable/xochitl/doc-1.pdf"}`)
	if !strings.Contains(first, `"text":"ok1"`) || !strings.Contains(first, `"done":true`) {
		t.Fatalf("first response=%s", first)
	}
	second := postAIPageChat(t, h, `{"prompt":"second prompt","document_id":"doc-1","document_path":"/home/root/.local/share/remarkable/xochitl/doc-1.pdf"}`)
	if !strings.Contains(second, `"text":"ok2"`) || !strings.Contains(second, `"done":true`) {
		t.Fatalf("second response=%s", second)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections=%d want 2", got)
	}

	var store codexThreadStore
	stored, err := os.ReadFile(filepath.Join(root, "codex_threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stored, &store); err != nil {
		t.Fatal(err)
	}
	if got := store.Documents["doc-1"].ThreadID; got != "thread-doc-1" {
		t.Fatalf("stored thread=%q want thread-doc-1; store=%s", got, stored)
	}
}

func TestAIPageChatRejectsInvalidDocumentID(t *testing.T) {
	_, h, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/ai-page-chat", strings.NewReader(`{"prompt":"hello","document_id":"../bad"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
}

func postAIPageChat(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/ai-page-chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
	return rr.Body.String()
}

func newFakeCodexAppServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		handleFakeCodexConnection(t, conn, int(connections.Add(1)))
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), &connections
}

type fakeCodexRequest struct {
	ID     *int            `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func handleFakeCodexConnection(t *testing.T, conn *websocket.Conn, index int) {
	t.Helper()
	initReq := readFakeCodexRequest(t, conn)
	if initReq.Method != "initialize" || initReq.ID == nil {
		t.Fatalf("first request=%+v want initialize", initReq)
	}
	initialized := readFakeCodexRequest(t, conn)
	if initialized.Method != "initialized" {
		t.Fatalf("second request=%+v want initialized", initialized)
	}
	writeFakeCodex(t, conn, map[string]any{"id": *initReq.ID, "result": map[string]any{}})

	threadReq := readFakeCodexRequest(t, conn)
	threadID := "thread-doc-1"
	switch index {
	case 1:
		if threadReq.Method != "thread/start" {
			t.Fatalf("request=%+v want thread/start", threadReq)
		}
		var params struct {
			Ephemeral bool   `json:"ephemeral"`
			Service   string `json:"serviceName"`
		}
		if err := json.Unmarshal(threadReq.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.Ephemeral || params.Service != "rmkit_cn" {
			t.Fatalf("thread/start params=%+v", params)
		}
	case 2:
		if threadReq.Method != "thread/resume" {
			t.Fatalf("request=%+v want thread/resume", threadReq)
		}
		var params struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(threadReq.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.ThreadID != threadID {
			t.Fatalf("resume thread=%q want %q", params.ThreadID, threadID)
		}
	default:
		t.Fatalf("unexpected connection %d", index)
	}
	writeFakeCodex(t, conn, map[string]any{
		"id": *threadReq.ID,
		"result": map[string]any{
			"thread": map[string]any{"id": threadID},
		},
	})

	turnReq := readFakeCodexRequest(t, conn)
	if turnReq.Method != "turn/start" || turnReq.ID == nil {
		t.Fatalf("request=%+v want turn/start", turnReq)
	}
	var turn struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(turnReq.Params, &turn); err != nil {
		t.Fatal(err)
	}
	if turn.ThreadID != threadID || len(turn.Input) != 1 {
		t.Fatalf("turn params=%+v", turn)
	}
	switch index {
	case 1:
		wantPrefix := "@/home/root/.local/share/remarkable/xochitl/doc-1.pdf\n\n"
		if !strings.HasPrefix(turn.Input[0].Text, wantPrefix) {
			t.Fatalf("first prompt=%q does not start with %q", turn.Input[0].Text, wantPrefix)
		}
	case 2:
		if strings.Contains(turn.Input[0].Text, "@/home/root/.local/share/remarkable/xochitl/doc-1.pdf") {
			t.Fatalf("second prompt unexpectedly included document path: %q", turn.Input[0].Text)
		}
		if turn.Input[0].Text != "second prompt" {
			t.Fatalf("second prompt=%q", turn.Input[0].Text)
		}
	}
	writeFakeCodex(t, conn, map[string]any{"id": *turnReq.ID, "result": map[string]any{}})
	delta := map[int]string{1: "ok1", 2: "ok2"}[index]
	writeFakeCodex(t, conn, map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{"threadId": threadID, "delta": delta},
	})
	writeFakeCodex(t, conn, map[string]any{
		"method": "turn/completed",
		"params": map[string]any{"threadId": threadID},
	})
}

func readFakeCodexRequest(t *testing.T, conn *websocket.Conn) fakeCodexRequest {
	t.Helper()
	var req fakeCodexRequest
	if err := conn.ReadJSON(&req); err != nil {
		t.Fatalf("read request: %v", err)
	}
	return req
}

func writeFakeCodex(t *testing.T, conn *websocket.Conn, msg map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write response: %v", err)
	}
}
