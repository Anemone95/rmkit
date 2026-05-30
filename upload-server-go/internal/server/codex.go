package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	codexDefaultURL             = "ws://127.0.0.1:48173"
	codexDefaultModel           = "gpt-5.5"
	codexDefaultReasoningEffort = "low"
)

var codexAppServerMu sync.Mutex

type codexRPCMessage struct {
	ID     *int            `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type codexInput []map[string]any

func isCodexKind(kind string) bool {
	return strings.EqualFold(strings.TrimSpace(kind), "codex")
}

func normalizeAIConfig(cfg aiConfig) aiConfig {
	cfg.Kind = strings.TrimSpace(strings.ToLower(cfg.Kind))
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.ReasoningEffort = strings.TrimSpace(cfg.ReasoningEffort)
	if cfg.Kind == "" {
		cfg.Kind = "codex"
	}
	if isCodexKind(cfg.Kind) {
		if cfg.URL == "" {
			cfg.URL = codexDefaultURL
		}
		if cfg.Model == "" {
			cfg.Model = codexDefaultModel
		}
		if effort, ok := normalizeCodexReasoningEffort(cfg.ReasoningEffort); ok {
			cfg.ReasoningEffort = effort
		} else {
			cfg.ReasoningEffort = codexDefaultReasoningEffort
		}
	}
	return cfg
}

func normalizeCodexReasoningEffort(effort string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return codexDefaultReasoningEffort, true
	case "low":
		return "low", true
	case "medium":
		return "medium", true
	case "high":
		return "high", true
	case "xhigh", "extra high", "extra-high", "extra_high":
		return "xhigh", true
	default:
		return "", false
	}
}

func (s *Server) readAIConfig() aiConfig {
	cfg := defaultAIConfig()
	if data, err := os.ReadFile(s.cfg.AIConfigPath); err == nil {
		var disk aiConfig
		if json.Unmarshal(data, &disk) == nil {
			cfg = disk
		}
	}
	return normalizeAIConfig(cfg)
}

func codexTextInput(text string) codexInput {
	return codexInput{{
		"type":          "text",
		"text":          text,
		"text_elements": []any{},
	}}
}

func codexTextImageInput(text, imagePath string) codexInput {
	input := codexTextInput(text)
	if imagePath != "" {
		input = append(input, map[string]any{
			"type":   "localImage",
			"path":   imagePath,
			"detail": "high",
		})
	}
	return input
}

func callCodex(ctx context.Context, cfg aiConfig, prompt string) (string, error) {
	var out strings.Builder
	err := callCodexStream(ctx, cfg, codexTextInput(prompt), func(chunk string) {
		out.WriteString(chunk)
	})
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", errors.New("Codex 响应为空")
	}
	return text, nil
}

func callCodexStream(ctx context.Context, cfg aiConfig, input codexInput, onChunk func(string)) error {
	codexAppServerMu.Lock()
	defer codexAppServerMu.Unlock()

	cfg = normalizeAIConfig(cfg)
	conn, err := openCodexWebSocket(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := &codexRPCClient{conn: conn}
	if err := client.initialize(ctx); err != nil {
		return err
	}
	threadID, err := client.startThread(ctx, cfg, true)
	if err != nil {
		return err
	}
	return client.startTurnAndStream(ctx, cfg, threadID, input, onChunk)
}

func (s *Server) callCodexDocumentStream(ctx context.Context, cfg aiConfig, documentID, documentPath, prompt string, onChunk func(string)) error {
	codexAppServerMu.Lock()
	defer codexAppServerMu.Unlock()

	cfg = normalizeAIConfig(cfg)
	conn, err := openCodexWebSocket(ctx, cfg.URL)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := &codexRPCClient{conn: conn}
	if err := client.initialize(ctx); err != nil {
		return err
	}

	rec, err := s.codexThreadForDocument(documentID)
	if err != nil {
		return err
	}
	threadID := strings.TrimSpace(rec.ThreadID)
	newThread := false
	if threadID != "" {
		if err := client.resumeThread(ctx, cfg, threadID); err != nil {
			fmt.Fprintf(os.Stderr, "Codex thread/resume doc=%s thread=%s failed: %v\n", documentID, threadID, err)
			threadID = ""
		}
	}
	if threadID == "" {
		threadID, err = client.startThread(ctx, cfg, false)
		if err != nil {
			return err
		}
		newThread = true
		if err := s.saveCodexThreadForDocument(documentID, codexThreadRecord{
			ThreadID:     threadID,
			DocumentPath: documentPath,
		}); err != nil {
			return err
		}
	}

	if newThread {
		prompt = codexInitialDocumentPrompt(ctx, prompt, documentPath, shouldAttachDocumentText(prompt))
	}
	return client.startTurnAndStream(ctx, cfg, threadID, codexTextInput(prompt), onChunk)
}

type codexRPCClient struct {
	conn   *websocket.Conn
	nextID int
}

func (c *codexRPCClient) sendRequest(method string, params map[string]any) (int, error) {
	c.nextID++
	id := c.nextID
	return id, c.conn.WriteJSON(map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	})
}

func (c *codexRPCClient) sendNotification(method string, params map[string]any) error {
	return c.conn.WriteJSON(map[string]any{
		"method": method,
		"params": params,
	})
}

func (c *codexRPCClient) readMessage(ctx context.Context) (codexRPCMessage, error) {
	var msg codexRPCMessage
	done := make(chan error, 1)
	go func() { done <- c.conn.ReadJSON(&msg) }()
	select {
	case <-ctx.Done():
		return msg, ctx.Err()
	case err := <-done:
		if err != nil {
			return msg, err
		}
		return msg, nil
	}
}

func (c *codexRPCClient) initialize(ctx context.Context) error {
	id, err := c.sendRequest("initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "rmkit_cn",
			"title":   "rmkit-cn",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err != nil {
		return fmt.Errorf("Codex initialize 发送失败: %w", err)
	}
	if err := c.sendNotification("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("Codex initialized 发送失败: %w", err)
	}
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return fmt.Errorf("Codex initialize 失败: %w", err)
		}
		if err := c.handleServerRequest(msg); err != nil {
			return err
		}
		if msg.ID != nil && *msg.ID == id {
			if len(msg.Error) > 0 {
				return fmt.Errorf("Codex initialize 错误: %s", snippet(msg.Error))
			}
			return nil
		}
	}
}

func (c *codexRPCClient) startThread(ctx context.Context, cfg aiConfig, ephemeral bool) (string, error) {
	params := map[string]any{
		"cwd":              "/home/root",
		"approvalPolicy":   "never",
		"sandbox":          "read-only",
		"serviceName":      "rmkit_cn",
		"ephemeral":        ephemeral,
		"baseInstructions": "你是 reMarkable 本地 AI 助手。只回答用户问题，不运行命令，不修改文件，不解释你的系统环境。默认用中文，除非用户明确要求其它语言。",
	}
	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	id, err := c.sendRequest("thread/start", params)
	if err != nil {
		return "", fmt.Errorf("Codex thread/start 发送失败: %w", err)
	}
	var threadID string
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return "", fmt.Errorf("Codex thread/start 失败: %w", err)
		}
		if err := c.handleServerRequest(msg); err != nil {
			return "", err
		}
		if msg.Method == "thread/started" {
			if id := extractThreadID(msg.Params); id != "" {
				threadID = id
			}
		}
		if msg.ID != nil && *msg.ID == id {
			if len(msg.Error) > 0 {
				return "", fmt.Errorf("Codex thread/start 错误: %s", snippet(msg.Error))
			}
			if id := extractThreadID(msg.Result); id != "" {
				threadID = id
			}
			if threadID == "" {
				return "", errors.New("Codex thread/start 没有返回 thread id")
			}
			return threadID, nil
		}
	}
}

func (c *codexRPCClient) resumeThread(ctx context.Context, cfg aiConfig, threadID string) error {
	params := map[string]any{
		"threadId":         threadID,
		"cwd":              "/home/root",
		"approvalPolicy":   "never",
		"sandbox":          "read-only",
		"baseInstructions": "你是 reMarkable 本地 AI 助手。只回答用户问题，不运行命令，不修改文件，不解释你的系统环境。默认用中文，除非用户明确要求其它语言。",
	}
	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	id, err := c.sendRequest("thread/resume", params)
	if err != nil {
		return fmt.Errorf("Codex thread/resume 发送失败: %w", err)
	}
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return fmt.Errorf("Codex thread/resume 失败: %w", err)
		}
		if err := c.handleServerRequest(msg); err != nil {
			return err
		}
		if msg.ID != nil && *msg.ID == id {
			if len(msg.Error) > 0 {
				return fmt.Errorf("Codex thread/resume 错误: %s", snippet(msg.Error))
			}
			return nil
		}
	}
}

func (c *codexRPCClient) startTurnAndStream(ctx context.Context, cfg aiConfig, threadID string, input codexInput, onChunk func(string)) error {
	params := map[string]any{
		"threadId":       threadID,
		"input":          input,
		"cwd":            "/home/root",
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "readOnly", "networkAccess": true},
	}
	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	if cfg.ReasoningEffort != "" {
		params["effort"] = cfg.ReasoningEffort
	}
	id, err := c.sendRequest("turn/start", params)
	if err != nil {
		return fmt.Errorf("Codex turn/start 发送失败: %w", err)
	}
	var streamText strings.Builder
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return fmt.Errorf("Codex turn/start 失败: %w", err)
		}
		if err := c.handleServerRequest(msg); err != nil {
			return err
		}
		if msg.ID != nil && *msg.ID == id && len(msg.Error) > 0 {
			return fmt.Errorf("Codex turn/start 错误: %s", snippet(msg.Error))
		}
		switch msg.Method {
		case "item/agentMessage/delta":
			var p struct {
				ThreadID string `json:"threadId"`
				Delta    string `json:"delta"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && (p.ThreadID == "" || p.ThreadID == threadID) && p.Delta != "" {
				streamText.WriteString(p.Delta)
				onChunk(p.Delta)
			}
		case "item/completed":
			var p struct {
				ThreadID string `json:"threadId"`
				Item     struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && p.Item.Type == "agentMessage" && p.Item.Text != "" {
				full := p.Item.Text
				prefix := streamText.String()
				if strings.HasPrefix(full, prefix) && len(full) > len(prefix) {
					chunk := full[len(prefix):]
					streamText.WriteString(chunk)
					onChunk(chunk)
				} else if prefix == "" {
					streamText.WriteString(full)
					onChunk(full)
				}
			}
		case "turn/completed":
			return nil
		case "error":
			return fmt.Errorf("Codex 错误: %s", snippet(msg.Params))
		}
	}
}

func (c *codexRPCClient) handleServerRequest(msg codexRPCMessage) error {
	if msg.ID == nil || msg.Method == "" || len(msg.Result) > 0 || len(msg.Error) > 0 {
		return nil
	}
	_ = c.conn.WriteJSON(map[string]any{
		"id": *msg.ID,
		"error": map[string]any{
			"code":    -32000,
			"message": "rmkit-cn 不允许 Codex 工具请求",
		},
	})
	return nil
}

func extractThreadID(raw json.RawMessage) string {
	var outer struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &outer) == nil && outer.Thread.ID != "" {
		return outer.Thread.ID
	}
	return ""
}

func openCodexWebSocket(ctx context.Context, url string) (*websocket.Conn, error) {
	if url == "" {
		url = codexDefaultURL
	}
	if conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil); err == nil {
		return conn, nil
	}
	if err := startCodexAppServer(ctx, url); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("Codex app-server 未就绪: %w", lastErr)
}

func startCodexAppServer(ctx context.Context, url string) error {
	path := os.Getenv("CODEX_BIN")
	if path == "" {
		if _, err := os.Stat("/home/root/.local/bin/codex"); err == nil {
			path = "/home/root/.local/bin/codex"
		} else {
			path = "codex"
		}
	}
	cmd := exec.Command(path, "app-server", "--listen", url)
	cmd.Env = append(os.Environ(),
		"HOME=/home/root",
		"CODEX_HOME=/home/root/.codex",
		"PATH=/home/root/.local/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	)
	cmd.SysProcAttr = newSessionLeader()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Codex app-server 失败: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
