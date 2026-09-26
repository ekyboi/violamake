package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

// Optical recognition reads chord symbols with OCR, and OCR on a short string
// in a serif face confuses letters that share a silhouette -- B read as E, for
// example. The mistake is invisible in the output: a wrong chord is still a
// valid chord, so nothing downstream can detect it.
//
// When the source PDF was engraved rather than scanned, it carries a text layer
// holding those same chord symbols exactly, as character codes rather than
// shapes. That text is ground truth, and comparing it against what recognition
// produced turns a silent error into a correctable one.
//
// This only helps for PDFs with a text layer. A scan has none, chordsFromPDF
// returns nothing, and the pipeline proceeds on recognition alone.

// chordText is one chord symbol read from the PDF, with the position it was
// drawn at. Systems are numbered from the top of the page.
type chordText struct {
	system int
	x      float64
	y      float64
	symbol string
}

// chordPattern matches a chord symbol: a root letter, an optional accidental,
// and an optional quality such as m, 7, m7, maj7, dim, sus4.
var chordPattern = regexp.MustCompile(`^[A-G][#b]?(m|maj|dim|aug|sus)?[0-9]?$`)

// chordsFromPDF extracts chord symbols from a PDF's text layer, grouped by the
// staff system they sit above. It returns nil for a PDF with no text layer,
// which is the normal case for a scan.
func chordsFromPDF(path string, staffTops []float64) ([]chordText, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading PDF text layer: %w", err)
	}
	defer f.Close()

	if r.NumPage() < 1 {
		return nil, nil
	}

	var all []chordText
	for pageNum := 1; pageNum <= r.NumPage(); pageNum++ {
		page := r.Page(pageNum)
		if page.V.IsNull() {
			continue
		}
		words := groupWords(page.Content().Text)

		for _, w := range words {
			if !chordPattern.MatchString(w.symbol) {
				continue
			}
			// A chord symbol sits just above a staff. Anything further away is
			// a title, a page number or a footer.
			sys := systemAbove(w.y, staffTops)
			if sys < 0 {
				continue
			}
			w.system = sys
			all = append(all, w)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].system != all[j].system {
			return all[i].system < all[j].system
		}
		return all[i].x < all[j].x
	})
	return all, nil
}

// groupWords joins the individual glyphs a PDF stores into whole words. Text is
// drawn character by character, so adjacent glyphs on the same baseline with no
// meaningful gap belong to one symbol.
func groupWords(texts []pdf.Text) []chordText {
	ts := make([]pdf.Text, 0, len(texts))
	for _, t := range texts {
		if strings.TrimSpace(t.S) != "" {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Y != ts[j].Y {
			return ts[i].Y > ts[j].Y
		}
		return ts[i].X < ts[j].X
	})

	var out []chordText
	var cur []pdf.Text

	flush := func() {
		if len(cur) == 0 {
			return
		}
		var sb strings.Builder
		for _, t := range cur {
			sb.WriteString(t.S)
		}
		if s := strings.TrimSpace(sb.String()); s != "" {
			out = append(out, chordText{x: cur[0].X, y: cur[0].Y, symbol: s})
		}
		cur = nil
	}

	const gapTolerance = 2.0 // points; wider than kerning, narrower than a space
	for _, t := range ts {
		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			if t.Y != prev.Y || t.X-(prev.X+prev.W) > gapTolerance {
				flush()
			}
		}
		cur = append(cur, t)
	}
	flush()
	return out
}

// systemAbove returns the index of the staff system a chord symbol belongs to,
// or -1 if it sits too far from any staff to be a chord.
func systemAbove(y float64, staffTops []float64) int {
	const (
		minRise = 2.0  // directly on the staff is a notation glyph, not a chord
		maxRise = 32.0 // beyond this it is a title or a header
	)
	for i, top := range staffTops {
		if rise := y - top; rise > minRise && rise < maxRise {
			return i
		}
	}
	return -1
}

// staffTopsFromXML derives one reference y-position per staff system. The PDF
// text layer gives chord positions in page coordinates, so the chords must be
// grouped by system before they can be compared with the recognizer's output.
//
// Rather than parse the PDF's vector graphics for staff lines, the systems are
// inferred from the chord symbols themselves: chord text clusters tightly on a
// handful of distinct baselines, one per system.
func inferSystems(words []chordText) []float64 {
	var ys []float64
	for _, w := range words {
		if chordPattern.MatchString(w.symbol) {
			ys = append(ys, w.y)
		}
	}
	if len(ys) == 0 {
		return nil
	}
	sort.Float64s(ys)

	// Collapse near-identical baselines into one entry per system.
	const sameSystem = 6.0
	tops := []float64{ys[0]}
	for _, y := range ys[1:] {
		if y-tops[len(tops)-1] > sameSystem {
			tops = append(tops, y)
		}
	}
	// Page coordinates run bottom-up; systems read top-down.
	for i, j := 0, len(tops)-1; i < j; i, j = i+1, j-1 {
		tops[i], tops[j] = tops[j], tops[i]
	}
	return tops
}

// chordsBySystem extracts chord symbols and groups them by system, in reading
// order. It is the form the reconciler consumes.
func chordsBySystem(path string) ([][]string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading PDF text layer: %w", err)
	}
	defer f.Close()

	var words []chordText
	for pageNum := 1; pageNum <= r.NumPage(); pageNum++ {
		page := r.Page(pageNum)
		if page.V.IsNull() {
			continue
		}
		for _, w := range groupWords(page.Content().Text) {
			if chordPattern.MatchString(w.symbol) {
				words = append(words, w)
			}
		}
	}
	if len(words) == 0 {
		return nil, nil // no text layer, or no chord symbols: nothing to reconcile
	}

	baselines := inferSystems(words)
	groups := make([][]chordText, len(baselines))
	for _, w := range words {
		for i, b := range baselines {
			if diff := w.y - b; diff > -3 && diff < 3 {
				groups[i] = append(groups[i], w)
				break
			}
		}
	}

	out := make([][]string, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].x < g[j].x })
		row := make([]string, 0, len(g))
		for _, w := range g {
			row = append(row, w.symbol)
		}
		out = append(out, row)
	}
	return out, nil
}

// chordCorrection replaces the root of one recognized chord. The index is the
// position of the harmony within its system, counting in reading order.
type chordCorrection struct {
	system int
	index  int
	from   string // what recognition produced, for reporting
	to     string // what the page actually says
	step   string // corrected root letter
	alter  int    // corrected root alteration
}

// reconcileChords compares the chord symbols recognition produced against those
// read from the PDF's text layer and returns the corrections to apply.
//
// The two sequences are aligned rather than matched by position, because
// recognition regularly misses chords entirely: a positional comparison would
// shift after the first omission and rewrite every chord that followed. An
// alignment tolerates gaps on either side, so only genuine disagreements are
// reported and a chord recognition simply missed is left absent rather than
// guessed into place.
func reconcileChords(fromPDF, fromOMR [][]string) []chordCorrection {
	var out []chordCorrection

	for sys := 0; sys < len(fromPDF) && sys < len(fromOMR); sys++ {
		for _, pair := range alignSequences(fromPDF[sys], fromOMR[sys]) {
			if pair.a < 0 || pair.b < 0 {
				continue // present on only one side; nothing to correct
			}
			truth, got := fromPDF[sys][pair.a], fromOMR[sys][pair.b]
			if truth == got {
				continue
			}
			step, alter, ok := parseChordRoot(truth)
			if !ok {
				continue
			}
			// Only the root is corrected. Quality (m, 7, dim) comes from the
			// recognizer's own reading of the same text and rewriting it from a
			// guessed kind string would trade one error for another.
			if gotStep, gotAlter, ok := parseChordRoot(got); ok &&
				gotStep == step && gotAlter == alter {
				continue // roots already agree; only the quality differs
			}
			out = append(out, chordCorrection{
				system: sys, index: pair.b,
				from: got, to: truth,
				step: step, alter: alter,
			})
		}
	}
	return out
}

// parseChordRoot splits a chord symbol into its root letter and alteration.
func parseChordRoot(symbol string) (step string, alter int, ok bool) {
	if symbol == "" {
		return "", 0, false
	}
	step = strings.ToUpper(symbol[:1])
	if _, known := semitoneOf[step]; !known {
		return "", 0, false
	}
	if len(symbol) > 1 {
		switch symbol[1] {
		case '#':
			alter = 1
		case 'b':
			alter = -1
		}
	}
	return step, alter, true
}

// pair is one aligned position; -1 marks a gap on that side.
type pair struct{ a, b int }

// alignSequences aligns two chord sequences, allowing gaps, so that omissions
// on either side do not shift everything after them. This is the standard
// longest-common-subsequence alignment.
func alignSequences(a, b []string) []pair {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			// An exact match is worth more than a mismatch, so the alignment
			// prefers to pair identical chords and leave genuine differences
			// adjacent to each other.
			score := 1
			if a[i-1] == b[j-1] {
				score = 2
			}
			dp[i][j] = max(dp[i-1][j-1]+score, max(dp[i-1][j], dp[i][j-1]))
		}
	}

	var out []pair
	i, j := n, m
	for i > 0 && j > 0 {
		score := 1
		if a[i-1] == b[j-1] {
			score = 2
		}
		switch {
		case dp[i][j] == dp[i-1][j-1]+score:
			out = append(out, pair{i - 1, j - 1})
			i, j = i-1, j-1
		case dp[i][j] == dp[i-1][j]:
			out = append(out, pair{i - 1, -1})
			i--
		default:
			out = append(out, pair{-1, j - 1})
			j--
		}
	}
	for ; i > 0; i-- {
		out = append(out, pair{i - 1, -1})
	}
	for ; j > 0; j-- {
		out = append(out, pair{-1, j - 1})
	}

	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// chordsFromMusicXML reads the chord symbols recognition produced, grouped by
// staff system in reading order so they can be aligned with the PDF's own text.
func chordsFromMusicXML(path string) ([][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading recognized score: %w", err)
	}

	var systems [][]string
	cur := []string{}
	started := false

	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }

	var step, kindText string
	var alter int
	inHarmony := false

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("scanning recognized score: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "print":
				for _, a := range t.Attr {
					if a.Name.Local == "new-system" && a.Value == "yes" {
						if started {
							systems = append(systems, cur)
							cur = []string{}
						}
					}
				}
				started = true
			case "harmony":
				inHarmony, step, alter, kindText = true, "", 0, ""
			case "root-step":
				if inHarmony {
					var v string
					if err := dec.DecodeElement(&v, &t); err == nil {
						step = strings.TrimSpace(v)
					}
				}
			case "root-alter":
				if inHarmony {
					var v int
					if err := dec.DecodeElement(&v, &t); err == nil {
						alter = v
					}
				}
			case "kind":
				if inHarmony {
					for _, a := range t.Attr {
						if a.Name.Local == "text" {
							kindText = a.Value
						}
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "harmony" && inHarmony {
				inHarmony = false
				if step != "" {
					cur = append(cur, formatChord(step, alter, kindText))
					started = true
				}
			}
		}
	}
	systems = append(systems, cur)
	return systems, nil
}

// formatChord renders a chord the way it is printed, so it can be compared
// directly with the text read off the page.
func formatChord(step string, alter int, kindText string) string {
	s := step
	switch {
	case alter > 0:
		s += "#"
	case alter < 0:
		s += "b"
	}
	return s + kindText
}
