package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rmkit-cn/upload-server/internal/textlayer"
)

func TestValidateDocID(t *testing.T) {
	valid := []string{"abc", "123", "doc-123-ABC"}
	for _, docID := range valid {
		if err := validateDocID(docID); err != nil {
			t.Fatalf("validateDocID(%q): %v", docID, err)
		}
	}
	invalid := []string{"", "../evil", "a/b", "a.b", "a_b"}
	for _, docID := range invalid {
		if err := validateDocID(docID); err == nil {
			t.Fatalf("validateDocID(%q) returned nil", docID)
		}
	}
}

func TestWritePagesDirReplacesStaleFiles(t *testing.T) {
	root := t.TempDir()
	pagesDir := filepath.Join(root, "doc")
	if err := os.MkdirAll(pagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pagesDir, "9.json"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := textlayer.Sidecar{
		Version: 1,
		Source:  "doc.pdf",
		Pages: []textlayer.PageSidecar{{
			Index:  0,
			Width:  100,
			Height: 200,
		}},
	}
	if err := writePagesDir(pagesDir, result); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pagesDir, "0.json")); err != nil {
		t.Fatalf("new page missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pagesDir, "9.json")); !os.IsNotExist(err) {
		t.Fatalf("stale page still exists or unexpected error: %v", err)
	}
}
