package server

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	codexDocumentTextTimeout        = 45 * time.Second
	codexDocumentTextMaxBytes       = 1 << 20
	codexDocumentTextMaxStderrBytes = 64 << 10
)

var (
	codexDocumentTextExtractor = extractCodexDocumentText
	htmlScriptStyleRE          = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>`)
	htmlTagRE                  = regexp.MustCompile(`(?s)<[^>]+>`)
)

type codexDocumentText struct {
	Text      string
	Truncated bool
}

type limitedStringBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedStringBuffer) Write(p []byte) (int, error) {
	if b.Len() < b.max {
		remaining := b.max - b.Len()
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = b.Buffer.Write(p[:remaining])
	}
	return len(p), nil
}

func shouldAttachDocumentText(prompt string) bool {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if prompt == "" {
		return false
	}
	if strings.Contains(prompt, "请帮我翻译这句话") || strings.Contains(prompt, "上一轮回答") {
		return false
	}
	actions := []string{"总结", "概括", "摘要", "讲讲", "介绍", "summarize", "summary"}
	targets := []string{"论文", "文档", "文件", "全文", "内容", "paper", "document", "article"}
	return containsAny(prompt, actions) && containsAny(prompt, targets)
}

func containsAny(s string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func codexInitialDocumentPrompt(ctx context.Context, prompt, documentPath string, includeText bool) string {
	documentPath = normalizeCodexDocumentPath(documentPath)
	if documentPath == "" {
		return prompt
	}
	var out strings.Builder
	out.WriteString("@")
	out.WriteString(documentPath)
	out.WriteString("\n\n")
	if includeText {
		text, err := codexDocumentTextExtractor(ctx, documentPath)
		if err != nil {
			out.WriteString("文档文本提取失败: ")
			out.WriteString(err.Error())
			out.WriteString("\n\n")
		} else if strings.TrimSpace(text.Text) != "" {
			out.WriteString("以下是 rmkit-cn 从当前文档提取的全文文本，请基于它回答用户问题。")
			if text.Truncated {
				out.WriteString("注意: 文本过长，已截断。")
			}
			out.WriteString("\n<document_text path=\"")
			out.WriteString(documentPath)
			out.WriteString("\">\n")
			out.WriteString(text.Text)
			out.WriteString("\n</document_text>\n\n")
		}
	}
	out.WriteString(prompt)
	return out.String()
}

func extractCodexDocumentText(ctx context.Context, documentPath string) (codexDocumentText, error) {
	switch strings.ToLower(filepath.Ext(documentPath)) {
	case ".pdf":
		return extractPDFDocumentText(ctx, documentPath)
	case ".epub":
		return extractEPUBDocumentText(documentPath)
	default:
		return codexDocumentText{}, nil
	}
}

func extractPDFDocumentText(ctx context.Context, documentPath string) (codexDocumentText, error) {
	ctx, cancel := context.WithTimeout(ctx, codexDocumentTextTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, pdftotextCommand(), "-enc", "UTF-8", documentPath, "-")
	stderr := &limitedStringBuffer{max: codexDocumentTextMaxStderrBytes}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return codexDocumentText{}, fmt.Errorf("pdftotext stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return codexDocumentText{}, fmt.Errorf("start pdftotext: %w", err)
	}
	outputCh := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(stdout, codexDocumentTextMaxBytes+1))
		outputCh <- struct {
			data []byte
			err  error
		}{data: data, err: err}
	}()

	out := <-outputCh
	truncated := len(out.data) > codexDocumentTextMaxBytes
	if truncated {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return codexDocumentText{}, fmt.Errorf("pdftotext timed out: %w", ctx.Err())
	}
	if out.err != nil {
		return codexDocumentText{}, fmt.Errorf("read pdftotext output: %w", out.err)
	}
	if waitErr != nil && !truncated {
		return codexDocumentText{}, fmt.Errorf("pdftotext failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	text, truncatedByUTF8 := truncateUTF8(string(out.data), codexDocumentTextMaxBytes)
	return codexDocumentText{
		Text:      cleanDocumentText(text),
		Truncated: truncated || truncatedByUTF8,
	}, nil
}

func pdftotextCommand() string {
	if path := strings.TrimSpace(os.Getenv("PDFTOTEXT_BIN")); path != "" {
		return path
	}
	if _, err := os.Stat("/home/root/rmkit-cn/bin/pdftotext"); err == nil {
		return "/home/root/rmkit-cn/bin/pdftotext"
	}
	return "pdftotext"
}

func extractEPUBDocumentText(documentPath string) (codexDocumentText, error) {
	zr, err := zip.OpenReader(documentPath)
	if err != nil {
		return codexDocumentText{}, fmt.Errorf("open epub: %w", err)
	}
	defer zr.Close()

	files := append([]*zip.File(nil), zr.File...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	var out strings.Builder
	truncated := false
	for _, f := range files {
		if !isEPUBTextFile(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return codexDocumentText{}, fmt.Errorf("read epub entry %s: %w", f.Name, err)
		}
		remaining := codexDocumentTextMaxBytes - out.Len()
		data, readErr := io.ReadAll(io.LimitReader(rc, int64(remaining+1)))
		closeErr := rc.Close()
		if readErr != nil {
			return codexDocumentText{}, fmt.Errorf("read epub entry %s: %w", f.Name, readErr)
		}
		if closeErr != nil {
			return codexDocumentText{}, fmt.Errorf("close epub entry %s: %w", f.Name, closeErr)
		}
		if len(data) > remaining {
			data = data[:remaining]
			truncated = true
		}
		chunk := stripMarkup(string(data))
		if strings.TrimSpace(chunk) != "" {
			if out.Len() > 0 {
				out.WriteString("\n\n")
			}
			out.WriteString(chunk)
		}
		if truncated || out.Len() >= codexDocumentTextMaxBytes {
			truncated = true
			break
		}
	}
	text, truncatedByUTF8 := truncateUTF8(out.String(), codexDocumentTextMaxBytes)
	return codexDocumentText{
		Text:      cleanDocumentText(text),
		Truncated: truncated || truncatedByUTF8,
	}, nil
}

func isEPUBTextFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "meta-inf/") {
		return false
	}
	ext := filepath.Ext(lower)
	return ext == ".xhtml" || ext == ".html" || ext == ".htm"
}

func stripMarkup(s string) string {
	s = htmlScriptStyleRE.ReplaceAllString(s, " ")
	s = htmlTagRE.ReplaceAllString(s, " ")
	return html.UnescapeString(s)
}

func cleanDocumentText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	var out []string
	blank := false
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func truncateUTF8(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut], true
}
