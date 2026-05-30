package textlayer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBuildsSentencesAndGeometry(t *testing.T) {
	const xml = `
<html xmlns="http://www.w3.org/1999/xhtml">
  <body>
    <doc>
      <page width="100" height="200">
        <flow>
          <block>
            <line>
              <word xMin="10" yMin="20" xMax="18" yMax="30">Dr.</word>
              <word xMin="20" yMin="20" xMax="40" yMax="30">Smith</word>
              <word xMin="42" yMin="20" xMax="65" yMax="30">writes</word>
              <word xMin="67" yMin="20" xMax="90" yMax="30">well.</word>
            </line>
            <line>
              <word xMin="10" yMin="40" xMax="45" yMax="50">Micro-</word>
            </line>
            <line>
              <word xMin="10" yMin="60" xMax="50" yMax="70">services</word>
              <word xMin="52" yMin="60" xMax="75" yMax="70">work</word>
              <word xMin="76" yMin="60" xMax="80" yMax="70">?</word>
            </line>
          </block>
          <block>
            <line>
              <word xMin="10" yMin="90" xMax="30" yMax="100">Next</word>
              <word xMin="32" yMin="90" xMax="55" yMax="100">block</word>
            </line>
          </block>
        </flow>
      </page>
    </doc>
  </body>
</html>`

	got, err := Parse("paper.pdf", []byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Source != "paper.pdf" {
		t.Fatalf("metadata=%+v", got)
	}
	if len(got.Pages) != 1 {
		t.Fatalf("pages=%d want 1", len(got.Pages))
	}
	page := got.Pages[0]
	if page.Index != 0 || page.Width != 100 || page.Height != 200 {
		t.Fatalf("page=%+v", page)
	}
	if len(page.Sentences) != 3 {
		t.Fatalf("sentences=%d want 3: %+v", len(page.Sentences), page.Sentences)
	}

	wantTexts := []string{
		"Dr. Smith writes well.",
		"Microservices work?",
		"Next block",
	}
	for i, want := range wantTexts {
		if page.Sentences[i].Text != want {
			t.Fatalf("sentence[%d]=%q want %q", i, page.Sentences[i].Text, want)
		}
	}

	firstArea := page.Sentences[0].Areas[0]
	if firstArea != (Rect{X: 9.5, Y: 19, W: 81, H: 12}) {
		t.Fatalf("first area=%+v", firstArea)
	}
	second := page.Sentences[1]
	if len(second.Areas) != 2 {
		t.Fatalf("second areas=%d want 2", len(second.Areas))
	}
	if second.BBox != (Rect{X: 9.5, Y: 39, W: 71, H: 32}) {
		t.Fatalf("second bbox=%+v", second.BBox)
	}
}

func TestParseRejectsInvalidXML(t *testing.T) {
	if _, err := Parse("bad.pdf", []byte("<html>")); err == nil {
		t.Fatal("Parse invalid XML returned nil error")
	}
}

func TestBuildWithContextRunsPdftotext(t *testing.T) {
	root := t.TempDir()
	pdfPath := filepath.Join(root, "paper.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdftotext := filepath.Join(root, "pdftotext")
	script := `#!/bin/sh
cat <<'EOF'
<html xmlns="http://www.w3.org/1999/xhtml"><body><doc><page width="10" height="20"><flow><block><line><word xMin="1" yMin="2" xMax="3" yMax="4">Hi.</word></line></block></flow></page></doc></body></html>
EOF
`
	if err := os.WriteFile(pdftotext, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := BuildWithContext(context.Background(), pdfPath, pdftotext)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "paper.pdf" || len(got.Pages) != 1 || got.Pages[0].Sentences[0].Text != "Hi." {
		t.Fatalf("sidecar=%+v", got)
	}
}

func TestBuildWithContextReportsPdftotextFailure(t *testing.T) {
	root := t.TempDir()
	pdftotext := filepath.Join(root, "pdftotext")
	if err := os.WriteFile(pdftotext, []byte("#!/bin/sh\necho broken >&2\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := BuildWithContext(context.Background(), filepath.Join(root, "paper.pdf"), pdftotext)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err=%v, want stderr detail", err)
	}
}
