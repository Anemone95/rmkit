package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/rmkit-cn/upload-server/internal/textlayer"
)

var docIDRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

const (
	xochitlDir      = "/home/root/.local/share/remarkable/xochitl"
	textLayerDir    = "/home/root/xovi/translate-text-layer"
	docIDUsageError = "doc-id must contain only letters, digits, and hyphen"
)

func validateDocID(docID string) error {
	if !docIDRE.MatchString(docID) {
		return fmt.Errorf(docIDUsageError)
	}
	return nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writePagesDir(path string, result textlayer.Sidecar) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(path)+".tmp.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	for _, page := range result.Pages {
		pageDoc := struct {
			Version int                   `json:"version"`
			Source  string                `json:"source"`
			Page    textlayer.PageSidecar `json:"page"`
		}{Version: result.Version, Source: result.Source, Page: page}
		if err := writeJSON(filepath.Join(tmp, fmt.Sprintf("%d.json", page.Index)), pageDoc); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func main() {
	docID := flag.String("doc-id", "", "reMarkable document id")
	pdfPath := flag.String("pdf", "", "source PDF path")
	outputPath := flag.String("output", "", "whole-document sidecar path")
	pagesDir := flag.String("pages-dir", "", "per-page sidecar output directory")
	pdftotext := flag.String("pdftotext", "/home/root/rmkit-cn/bin/pdftotext", "pdftotext executable")
	flag.Parse()

	if *docID != "" {
		if err := validateDocID(*docID); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if *pdfPath == "" {
			*pdfPath = filepath.Join(xochitlDir, *docID+".pdf")
		}
		if *outputPath == "" {
			*outputPath = filepath.Join(textLayerDir, *docID+".json")
		}
		if *pagesDir == "" {
			*pagesDir = filepath.Join(textLayerDir, *docID)
		}
	}
	if *pdfPath == "" || *outputPath == "" || *pagesDir == "" {
		fmt.Fprintln(os.Stderr, "usage: text-layer-sidecar -doc-id <id> [-pdftotext <path>]")
		os.Exit(2)
	}
	result, err := textlayer.Build(*pdfPath, *pdftotext)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writeJSON(*outputPath, result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writePagesDir(*pagesDir, result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d pages for %s\n", len(result.Pages), *pdfPath)
}
