package main

import (
	"strings"
	"testing"
)

// semitonesBelow confirms the mapping really lowers pitch by a perfect fifth.
func absSemitone(step string, alter, octave int) int {
	return (octave+1)*12 + semitoneOf[step] + alter
}

func TestFifthDownIsExactlySevenSemitones(t *testing.T) {
	for _, step := range []string{"C", "D", "E", "F", "G", "A", "B"} {
		for _, alter := range []int{-1, 0, 1} {
			for octave := 2; octave <= 7; octave++ {
				a := alter
				p := pitch{Step: step, Alter: &a, Octave: octave}
				before := absSemitone(step, alter, octave)

				if _, err := transposePitch(&p); err != nil {
					t.Fatalf("%s%+d/%d: %v", step, alter, octave, err)
				}

				na := 0
				if p.Alter != nil {
					na = *p.Alter
				}
				after := absSemitone(p.Step, na, p.Octave)

				if before-after != 7 {
					t.Errorf("%s%+d/%d -> %s%+d/%d: fell %d semitones, want 7",
						step, alter, octave, p.Step, na, p.Octave, before-after)
				}
			}
		}
	}
}

// The open strings are the whole point: violin E-A-D-G must land on viola
// A-D-G-C at the same written position.
func TestOpenStringMapping(t *testing.T) {
	cases := []struct {
		step string
		oct  int
		want string
		wOct int
	}{
		{"E", 5, "A", 4}, // E5 -> A4
		{"A", 4, "D", 4}, // A4 -> D4
		{"D", 4, "G", 3}, // D4 -> G3
		{"G", 3, "C", 3}, // G3 -> C3
	}
	for _, c := range cases {
		p := pitch{Step: c.step, Octave: c.oct}
		if _, err := transposePitch(&p); err != nil {
			t.Fatalf("%s%d: %v", c.step, c.oct, err)
		}
		if p.Step != c.want || p.Octave != c.wOct {
			t.Errorf("%s%d -> %s%d, want %s%d", c.step, c.oct, p.Step, p.Octave, c.want, c.wOct)
		}
	}
}

// F is the tritone exception: a perfect fifth below F is B-flat, not B.
func TestFMapsToBFlat(t *testing.T) {
	p := pitch{Step: "F", Octave: 4}
	if _, err := transposePitch(&p); err != nil {
		t.Fatal(err)
	}
	if p.Step != "B" || p.Alter == nil || *p.Alter != -1 || p.Octave != 3 {
		t.Errorf("F4 -> %s alter=%v oct=%d, want B-flat octave 3", p.Step, p.Alter, p.Octave)
	}

	// F-sharp must become B-natural, with the <alter> dropped entirely.
	sharp := 1
	q := pitch{Step: "F", Alter: &sharp, Octave: 4}
	if _, err := transposePitch(&q); err != nil {
		t.Fatal(err)
	}
	if q.Step != "B" || q.Alter != nil {
		t.Errorf("F#4 -> %s alter=%v, want B natural with no alter", q.Step, q.Alter)
	}
}

func TestClefAndKeyRewrite(t *testing.T) {
	const score = `<?xml version="1.0"?>
<score-partwise><part id="P1"><measure number="1">
<attributes>
  <key><fifths>1</fifths><mode>major</mode></key>
  <clef><sign>G</sign><line>2</line></clef>
</attributes>
<note><pitch><step>E</step><octave>5</octave></pitch></note>
<note><pitch><step>F</step><alter>1</alter><octave>4</octave></pitch></note>
</measure></part></score-partwise>`

	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()

	if !strings.Contains(got, "<sign>C</sign>") || !strings.Contains(got, "<line>3</line>") {
		t.Errorf("clef was not converted to alto:\n%s", got)
	}
	if !strings.Contains(got, "<fifths>0</fifths>") {
		t.Errorf("key signature should move one step flatward (G major -> C major):\n%s", got)
	}
	if st.Clefs != 1 || st.Pitches != 2 || st.KeyFifths != 1 {
		t.Errorf("stats = %+v, want 1 clef / 2 pitches / 1 key", st)
	}
}

// A bass clef in the source is legitimate and must survive untouched.
func TestBassClefUntouched(t *testing.T) {
	const score = `<score-partwise><clef><sign>F</sign><line>4</line></clef></score-partwise>`
	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Clefs != 0 || !strings.Contains(out.String(), "<sign>F</sign>") {
		t.Errorf("bass clef should be preserved, got:\n%s", out.String())
	}
}

func TestPreservePitchOnlySwapsClef(t *testing.T) {
	const score = `<score-partwise>
<clef><sign>G</sign><line>2</line></clef>
<note><pitch><step>E</step><octave>5</octave></pitch></note>
</score-partwise>`

	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, true)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "<sign>C</sign>") {
		t.Error("clef should still be swapped under --preserve-pitch")
	}
	if !strings.Contains(got, "<step>E</step>") || st.Pitches != 0 {
		t.Errorf("pitches must be untouched under --preserve-pitch:\n%s", got)
	}
}

func TestDeriveOutput(t *testing.T) {
	cases := map[string]string{
		"/m/sonata.pdf":       "/m/sonata-viola.pdf",
		"/m/sonata.musicxml":  "/m/sonata-viola.pdf",
		"/m/sonata-viola.pdf": "/m/sonata-viola.pdf", // no double suffix
	}
	for in, want := range cases {
		if got := deriveOutput(in); got != want {
			t.Errorf("deriveOutput(%q) = %q, want %q", in, got, want)
		}
	}
}

// Chord symbols must move with the notes; a part sounding in G major that
// still prints D-major chord symbols is unusable.
func TestHarmonyTransposition(t *testing.T) {
	const score = `<score-partwise>
<harmony><root><root-step>D</root-step></root><kind>major</kind></harmony>
<harmony><root><root-step>A</root-step></root><kind>dominant</kind></harmony>
<harmony><root><root-step>E</root-step></root><kind>minor</kind></harmony>
</score-partwise>`

	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()

	for _, want := range []string{"<root-step>G</root-step>", "<root-step>D</root-step>", "<root-step>A</root-step>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if st.Harmonies != 3 {
		t.Errorf("Harmonies = %d, want 3", st.Harmonies)
	}
}

// An F chord must become B-flat, matching the pitch-level tritone exception.
func TestHarmonyFBecomesBFlat(t *testing.T) {
	const score = `<score-partwise>
<harmony><root><root-step>F</root-step><root-alter>0</root-alter></root></harmony>
</score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "<root-step>B</root-step>") || !strings.Contains(got, "<root-alter>-1</root-alter>") {
		t.Errorf("F chord should become B-flat, got:\n%s", got)
	}
}

// Under --preserve-pitch nothing harmonic moves, since the pitches do not.
func TestHarmonyUntouchedWhenPreservingPitch(t *testing.T) {
	const score = `<score-partwise><harmony><root><root-step>D</root-step></root></harmony></score-partwise>`
	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Harmonies != 0 || !strings.Contains(out.String(), "<root-step>D</root-step>") {
		t.Errorf("chords must not move under --preserve-pitch:\n%s", out.String())
	}
}
