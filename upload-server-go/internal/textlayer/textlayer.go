package textlayer

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	DefaultBuildTimeout     = 2 * time.Minute
	MaxPdftotextXMLBytes    = 64 << 20
	MaxPdftotextStderrBytes = 64 << 10
)

var (
	abbreviations = map[string]bool{
		"al.": true, "approx.": true, "cf.": true, "dr.": true, "e.g.": true,
		"eq.": true, "eqs.": true, "etc.": true, "fig.": true, "figs.": true,
		"i.e.": true, "mr.": true, "mrs.": true, "ms.": true, "no.": true,
		"prof.": true, "sec.": true, "secs.": true, "vs.": true,
	}
	initialRE = regexp.MustCompile(`^[A-Z]\.$`)
)

type htmlDoc struct {
	Pages []pageXML `xml:"body>doc>page"`
}

type pageXML struct {
	Width  float64   `xml:"width,attr"`
	Height float64   `xml:"height,attr"`
	Flows  []flowXML `xml:"flow"`
}

type flowXML struct {
	Blocks []blockXML `xml:"block"`
}

type blockXML struct {
	Lines []lineXML `xml:"line"`
}

type lineXML struct {
	Words []wordXML `xml:"word"`
}

type wordXML struct {
	XMin float64 `xml:"xMin,attr"`
	YMin float64 `xml:"yMin,attr"`
	XMax float64 `xml:"xMax,attr"`
	YMax float64 `xml:"yMax,attr"`
	Text string  `xml:",chardata"`
}

type word struct {
	Text string
	Line int
	X    float64
	Y    float64
	W    float64
	H    float64
}

type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type Sentence struct {
	Text  string `json:"text"`
	Areas []Rect `json:"areas"`
	BBox  Rect   `json:"bbox"`
}

type PageSidecar struct {
	Index     int        `json:"index"`
	Width     float64    `json:"width"`
	Height    float64    `json:"height"`
	Sentences []Sentence `json:"sentences"`
}

type Sidecar struct {
	Version int           `json:"version"`
	Source  string        `json:"source"`
	Pages   []PageSidecar `json:"pages"`
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len() < b.max {
		remaining := b.max - b.Len()
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = b.Buffer.Write(p[:remaining])
	}
	return len(p), nil
}

func Build(pdfPath, pdftotext string) (Sidecar, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultBuildTimeout)
	defer cancel()
	return BuildWithContext(ctx, pdfPath, pdftotext)
}

func BuildWithContext(ctx context.Context, pdfPath, pdftotext string) (Sidecar, error) {
	cmd := exec.CommandContext(ctx, pdftotext, "-bbox-layout", "-enc", "UTF-8", pdfPath, "-")
	stderr := &limitedBuffer{max: MaxPdftotextStderrBytes}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Sidecar{}, fmt.Errorf("pdftotext stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Sidecar{}, fmt.Errorf("start pdftotext: %w", err)
	}
	outputCh := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(stdout, MaxPdftotextXMLBytes+1))
		outputCh <- struct {
			data []byte
			err  error
		}{data: data, err: err}
	}()

	out := <-outputCh
	if len(out.data) > MaxPdftotextXMLBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Sidecar{}, fmt.Errorf("pdftotext output too large (>%d bytes)", MaxPdftotextXMLBytes)
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return Sidecar{}, fmt.Errorf("pdftotext timed out: %w", ctx.Err())
	}
	if out.err != nil {
		return Sidecar{}, fmt.Errorf("read pdftotext output: %w", out.err)
	}
	if waitErr != nil {
		return Sidecar{}, fmt.Errorf("pdftotext failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return Parse(filepath.Base(pdfPath), out.data)
}

func Parse(source string, data []byte) (Sidecar, error) {
	var doc htmlDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Sidecar{}, fmt.Errorf("parse pdftotext XML: %w", err)
	}
	result := Sidecar{Version: 1, Source: source}
	for index, page := range doc.Pages {
		result.Pages = append(result.Pages, PageSidecar{
			Index:     index,
			Width:     page.Width,
			Height:    page.Height,
			Sentences: pageSentences(page),
		})
	}
	return result, nil
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func sentenceEnds(words []word) bool {
	if len(words) == 0 {
		return false
	}
	candidate := strings.Trim(strings.TrimSpace(words[len(words)-1].Text), "\"'”’)]}")
	if !(strings.HasSuffix(candidate, ".") || strings.HasSuffix(candidate, "?") || strings.HasSuffix(candidate, "!")) {
		return false
	}
	if abbreviations[strings.ToLower(candidate)] || initialRE.MatchString(candidate) {
		return false
	}
	if len(words) >= 2 {
		pair := strings.ToLower(words[len(words)-2].Text + " " + words[len(words)-1].Text)
		if pair == "et al." || pair == "e. g." || pair == "i. e." {
			return false
		}
	}
	return true
}

func joinWords(words []word) string {
	var pieces []string
	var previous *word
	for i := range words {
		w := words[i]
		switch {
		case len(pieces) == 0:
			pieces = append(pieces, w.Text)
		case previous != nil && strings.HasSuffix(pieces[len(pieces)-1], "-") && previous.Line != w.Line:
			pieces[len(pieces)-1] = strings.TrimSuffix(pieces[len(pieces)-1], "-") + w.Text
		case startsWithPunctuation(w.Text):
			pieces[len(pieces)-1] += w.Text
		default:
			pieces = append(pieces, w.Text)
		}
		previous = &words[i]
	}
	return strings.ReplaceAll(strings.Join(pieces, " "), "\u00ad", "")
}

func startsWithPunctuation(s string) bool {
	for _, r := range s {
		return strings.ContainsRune(",.;:!?%)]}", r)
	}
	return false
}

func lineAreas(words []word) []Rect {
	byLine := make(map[int][]word)
	var lines []int
	for _, w := range words {
		if _, ok := byLine[w.Line]; !ok {
			lines = append(lines, w.Line)
		}
		byLine[w.Line] = append(byLine[w.Line], w)
	}
	sort.Ints(lines)
	areas := make([]Rect, 0, len(lines))
	for _, lineID := range lines {
		lineWords := byLine[lineID]
		xMin, yMin := lineWords[0].X, lineWords[0].Y
		xMax, yMax := lineWords[0].X+lineWords[0].W, lineWords[0].Y+lineWords[0].H
		for _, w := range lineWords[1:] {
			xMin = math.Min(xMin, w.X)
			yMin = math.Min(yMin, w.Y)
			xMax = math.Max(xMax, w.X+w.W)
			yMax = math.Max(yMax, w.Y+w.H)
		}
		areas = append(areas, Rect{
			X: round3(xMin - 0.5),
			Y: round3(yMin - 1.0),
			W: round3(xMax - xMin + 1.0),
			H: round3(yMax - yMin + 2.0),
		})
	}
	return areas
}

func boundingArea(areas []Rect) Rect {
	xMin, yMin := areas[0].X, areas[0].Y
	xMax, yMax := areas[0].X+areas[0].W, areas[0].Y+areas[0].H
	for _, area := range areas[1:] {
		xMin = math.Min(xMin, area.X)
		yMin = math.Min(yMin, area.Y)
		xMax = math.Max(xMax, area.X+area.W)
		yMax = math.Max(yMax, area.Y+area.H)
	}
	return Rect{X: round3(xMin), Y: round3(yMin), W: round3(xMax - xMin), H: round3(yMax - yMin)}
}

func sentenceRecord(words []word) Sentence {
	areas := lineAreas(words)
	return Sentence{Text: joinWords(words), Areas: areas, BBox: boundingArea(areas)}
}

func pageSentences(page pageXML) []Sentence {
	var sentences []Sentence
	lineID := 0
	for _, flow := range page.Flows {
		for _, block := range flow.Blocks {
			var current []word
			for _, line := range block.Lines {
				for _, xmlWord := range line.Words {
					text := strings.TrimSpace(xmlWord.Text)
					if text == "" {
						continue
					}
					current = append(current, word{
						Text: text,
						Line: lineID,
						X:    xmlWord.XMin,
						Y:    xmlWord.YMin,
						W:    xmlWord.XMax - xmlWord.XMin,
						H:    xmlWord.YMax - xmlWord.YMin,
					})
					if sentenceEnds(current) {
						sentences = append(sentences, sentenceRecord(current))
						current = nil
					}
				}
				lineID++
			}
			if len(current) > 0 {
				sentences = append(sentences, sentenceRecord(current))
			}
		}
	}
	return sentences
}
