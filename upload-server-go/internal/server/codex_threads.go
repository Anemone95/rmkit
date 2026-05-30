package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var (
	codexDocumentIDRE  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	codexThreadStoreMu sync.Mutex
)

type codexThreadStore struct {
	Version   int                          `json:"version"`
	Documents map[string]codexThreadRecord `json:"documents"`
}

type codexThreadRecord struct {
	ThreadID     string `json:"thread_id"`
	DocumentPath string `json:"document_path,omitempty"`
}

func normalizeCodexDocumentID(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false
	}
	if !codexDocumentIDRE.MatchString(id) {
		return "", false
	}
	return id, true
}

func normalizeCodexDocumentPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || strings.ContainsAny(path, "\r\n") || !strings.HasPrefix(path, XochitlDir+"/") {
		return ""
	}
	if len(path) > 512 {
		return ""
	}
	return path
}

func normalizeCodexDocumentPathForDocument(documentID, path string) string {
	path = normalizeCodexDocumentPath(path)
	if path == "" || documentID == "" {
		return path
	}
	base := XochitlDir + "/" + documentID
	if path == base || strings.HasPrefix(path, base+".") {
		return path
	}
	return ""
}

func codexInitialDocumentPrompt(prompt, documentPath string) string {
	documentPath = normalizeCodexDocumentPath(documentPath)
	if documentPath == "" {
		return prompt
	}
	return "@" + documentPath + "\n\n" + prompt
}

func (s *Server) codexThreadForDocument(documentID string) (codexThreadRecord, error) {
	codexThreadStoreMu.Lock()
	defer codexThreadStoreMu.Unlock()

	store, err := s.readCodexThreadStoreLocked()
	if err != nil {
		return codexThreadRecord{}, err
	}
	return store.Documents[documentID], nil
}

func (s *Server) saveCodexThreadForDocument(documentID string, rec codexThreadRecord) error {
	codexThreadStoreMu.Lock()
	defer codexThreadStoreMu.Unlock()

	store, err := s.readCodexThreadStoreLocked()
	if err != nil {
		return err
	}
	if store.Documents == nil {
		store.Documents = make(map[string]codexThreadRecord)
	}
	rec.ThreadID = strings.TrimSpace(rec.ThreadID)
	rec.DocumentPath = normalizeCodexDocumentPath(rec.DocumentPath)
	if rec.ThreadID == "" {
		delete(store.Documents, documentID)
	} else {
		store.Documents[documentID] = rec
	}
	return s.writeCodexThreadStoreLocked(store)
}

func (s *Server) readCodexThreadStoreLocked() (codexThreadStore, error) {
	store := codexThreadStore{
		Version:   1,
		Documents: make(map[string]codexThreadRecord),
	}
	data, err := os.ReadFile(s.cfg.CodexThreadsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return store, fmt.Errorf("读取 Codex 文档 thread 映射失败: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return store, fmt.Errorf("解析 Codex 文档 thread 映射失败: %w", err)
	}
	if store.Version == 0 {
		store.Version = 1
	}
	if store.Documents == nil {
		store.Documents = make(map[string]codexThreadRecord)
	}
	return store, nil
}

func (s *Server) writeCodexThreadStoreLocked(store codexThreadStore) error {
	if store.Version == 0 {
		store.Version = 1
	}
	if store.Documents == nil {
		store.Documents = make(map[string]codexThreadRecord)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 Codex 文档 thread 映射失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.CodexThreadsPath), 0o755); err != nil {
		return fmt.Errorf("创建 Codex 文档 thread 映射目录失败: %w", err)
	}
	tmp := s.cfg.CodexThreadsPath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 Codex 文档 thread 映射失败: %w", err)
	}
	if err := os.Rename(tmp, s.cfg.CodexThreadsPath); err != nil {
		return fmt.Errorf("保存 Codex 文档 thread 映射失败: %w", err)
	}
	return nil
}
