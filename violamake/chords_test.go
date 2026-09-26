package main

import (
	"reflect"
	"strings"
	"testing"
)

// Alignment must tolerate chords that recognition missed entirely. Comparing by
// position would shift after the first omission and rewrite everything after it.
func TestAlignmentToleratesMissingChords(t *testing.T) {
	fromPDF := [][]string{{"D", "G", "D", "A7"}}
	fromOMR := [][]string{{"D", "G", "D"}} // A7 dropped by recognition

	if got := reconcileChords(fromPDF, fromOMR); len(got) != 0 {
		t.Errorf("a chord recognition missed is not a disagreement, got %+v", got)
	}
}

// The real failure this exists for: a B misread as an E.
func TestMisreadRootIsCorrected(t *testing.T) {
	fromPDF := [][]string{{"D", "Bm", "E7"}}
	fromOMR := [][]string{{"D", "Em", "E7"}}

	got := reconcileChords(fromPDF, fromOMR)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 correction, got %d: %+v", len(got), got)
	}
	if got[0].step != "B" || got[0].index != 1 {
		t.Errorf("correction = %+v, want root B at index 1", got[0])
	}
}

// A misread root next to a missing chord must still be found, and must not drag
// the chords around it into false corrections.
func TestCorrectionAmidOmissions(t *testing.T) {
	fromPDF := [][]string{{"D", "Bm", "E7", "A7"}}
	fromOMR := [][]string{{"D", "Em", "E7"}}

	got := reconcileChords(fromPDF, fromOMR)
	if len(got) != 1 || got[0].to != "Bm" {
		t.Errorf("want only the Bm correction, got %+v", got)
	}
}

// Accidentals in the root must survive the round trip.
func TestChordRootParsing(t *testing.T) {
	cases := map[string]struct {
		step  string
		alter int
	}{
		"C": {"C", 0}, "F#": {"F", 1}, "Bb": {"B", -1},
		"Am": {"A", 0}, "C#m7": {"C", 1},
	}
	for in, want := range cases {
		step, alter, ok := parseChordRoot(in)
		if !ok || step != want.step || alter != want.alter {
			t.Errorf("parseChordRoot(%q) = %q,%d,%v; want %q,%d", in, step, alter, ok, want.step, want.alter)
		}
	}
	if _, _, ok := parseChordRoot("H"); ok {
		t.Error("H is not a chord root")
	}
}

// Quality differences alone are not root errors and must not be rewritten: the
// quality comes from the recognizer's reading of the same text.
func TestQualityDifferenceIsNotARootCorrection(t *testing.T) {
	fromPDF := [][]string{{"Am7"}}
	fromOMR := [][]string{{"Am"}}

	if got := reconcileChords(fromPDF, fromOMR); len(got) != 0 {
		t.Errorf("only roots are corrected, got %+v", got)
	}
}

// A corrected root must be what gets transposed, not the misread one.
func TestCorrectionAppliedBeforeTransposition(t *testing.T) {
	const score = `<score-partwise><part id="P1"><measure number="1">
<harmony><root><root-step>E</root-step></root><kind text="m">minor</kind></harmony>
</measure></part></score-partwise>`

	corrections := []chordCorrection{{system: 0, index: 0, from: "Em", to: "Bm", step: "B"}}

	var out strings.Builder
	st, err := TransposeWithCorrections(strings.NewReader(score), &out, false, corrections)
	if err != nil {
		t.Fatal(err)
	}
	// B corrected from E, then transposed down a fifth, gives E.
	if !strings.Contains(out.String(), "<root-step>E</root-step>") {
		t.Errorf("Bm should transpose to Em:\n%s", out.String())
	}
	if st.CorrectedChords != 1 {
		t.Errorf("CorrectedChords = %d, want 1", st.CorrectedChords)
	}
}

// With no corrections the behaviour is unchanged, which is the path taken for
// scanned PDFs and for MusicXML input.
func TestNoCorrectionsLeavesChordsAlone(t *testing.T) {
	const score = `<score-partwise><harmony><root><root-step>E</root-step></root></harmony></score-partwise>`

	var out strings.Builder
	st, err := TransposeWithCorrections(strings.NewReader(score), &out, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "<root-step>A</root-step>") || st.CorrectedChords != 0 {
		t.Errorf("E should simply transpose to A:\n%s", out.String())
	}
}

// Systems are compared pairwise; a page whose system count differs between the
// two sources must not panic or read past the end of either.
func TestMismatchedSystemCounts(t *testing.T) {
	got := reconcileChords([][]string{{"D"}, {"G"}, {"A"}}, [][]string{{"D"}})
	if !reflect.DeepEqual(got, []chordCorrection(nil)) && len(got) != 0 {
		t.Errorf("want no corrections, got %+v", got)
	}
}
