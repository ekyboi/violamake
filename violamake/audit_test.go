package main

import (
	"strings"
	"testing"
)

// The printed accidental must agree with the transposed pitch. Left stale, an
// F-sharp mapped to B would still print a sharp and engrave as B-sharp --
// a note a semitone above what the violin part sounded.
func TestAccidentalFollowsPitch(t *testing.T) {
	const score = `<score-partwise><note>
<pitch><step>F</step><alter>1</alter><octave>4</octave></pitch>
<accidental>sharp</accidental></note></score-partwise>`

	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "<step>B</step>") {
		t.Errorf("F#4 should transpose to B3:\n%s", got)
	}
	if !strings.Contains(got, "<accidental>natural</accidental>") {
		t.Errorf("accidental should become natural, not stay sharp:\n%s", got)
	}
	if st.Accidentals != 1 {
		t.Errorf("Accidentals = %d, want 1", st.Accidentals)
	}
}

// F natural becomes B-flat, so its accidental must appear even though the
// source note carried none.
func TestAccidentalForTritoneException(t *testing.T) {
	const score = `<score-partwise><note>
<pitch><step>F</step><octave>4</octave></pitch>
<accidental>natural</accidental></note></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "<accidental>flat</accidental>") {
		t.Errorf("F-natural -> B-flat should print a flat:\n%s", got)
	}
}

// Each note in a chord carries its own accidental; they must not cross-talk.
func TestAccidentalsPerChordNote(t *testing.T) {
	const score = `<score-partwise>
<note><pitch><step>F</step><alter>1</alter><octave>4</octave></pitch><accidental>sharp</accidental></note>
<note><chord/><pitch><step>C</step><alter>1</alter><octave>5</octave></pitch><accidental>sharp</accidental></note>
</score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// F#->B natural, C#->F# (keeps its sharp)
	if !strings.Contains(got, "<accidental>natural</accidental>") ||
		!strings.Contains(got, "<accidental>sharp</accidental>") {
		t.Errorf("chord notes should keep independent accidentals:\n%s", got)
	}
}

// encoding/xml resolves namespaces onto every name; re-encoding naively emits
// the xmlns declaration twice and produces malformed XML.
func TestNamespaceNotDuplicated(t *testing.T) {
	const score = `<score-partwise xmlns="http://www.musicxml.org/ns">
<clef><sign>G</sign><line>2</line></clef></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "xmlns="); n != 1 {
		t.Errorf("xmlns appears %d times, want 1:\n%s", n, out.String())
	}
}

// <key-octave> has no field on the key struct; without raw capture it would be
// silently dropped from the score.
func TestKeyOctavePreserved(t *testing.T) {
	const score = `<score-partwise><key><fifths>2</fifths><mode>major</mode>
<key-octave number="1">4</key-octave></key></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "key-octave") {
		t.Errorf("<key-octave> was dropped:\n%s", got)
	}
	if !strings.Contains(got, "<fifths>1</fifths>") {
		t.Errorf("key should move one step flatward:\n%s", got)
	}
}

// Seven flats going down a fifth would need eight; that is unwritable, so it
// wraps enharmonically to four sharps.
func TestKeySignatureWrapsAtSevenFlats(t *testing.T) {
	const score = `<score-partwise><key><fifths>-7</fifths></key></score-partwise>`
	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "<fifths>4</fifths>") {
		t.Errorf("-7 should wrap to 4:\n%s", out.String())
	}
}

// A value outside the real circle of fifths is not a key signature; it is left
// alone rather than shifted into a different kind of wrong.
func TestNonsenseKeyLeftAlone(t *testing.T) {
	const score = `<score-partwise><key><fifths>99</fifths></key></score-partwise>`
	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "<fifths>99</fifths>") || st.KeyFifths != 0 {
		t.Errorf("out-of-range fifths should pass through untouched:\n%s", out.String())
	}
}

// Attributes such as staff number must survive the decode/encode round trip.
func TestClefAttributesPreserved(t *testing.T) {
	const score = `<score-partwise><clef number="2" print-object="yes">
<sign>G</sign><line>2</line></clef></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `number="2"`) || !strings.Contains(got, `print-object="yes"`) {
		t.Errorf("clef attributes were lost:\n%s", got)
	}
}

// Rests and unpitched percussion have no <pitch> and must pass through cleanly.
func TestRestsAndUnpitchedUntouched(t *testing.T) {
	const score = `<score-partwise>
<note><rest/><duration>4</duration></note>
<note><unpitched><display-step>E</display-step><display-octave>4</display-octave></unpitched></note>
</score-partwise>`

	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pitches != 0 {
		t.Errorf("Pitches = %d, want 0", st.Pitches)
	}
	if !strings.Contains(out.String(), "display-step") {
		t.Errorf("unpitched content was lost:\n%s", out.String())
	}
}

// A pitch that would leave the engravable octave range is reported rather than
// written out as data no renderer can use.
func TestOctaveRangeIsEnforced(t *testing.T) {
	p := pitch{Step: "C", Octave: 0}
	if _, err := transposePitch(&p); err == nil {
		t.Errorf("C0 transposed down a fifth leaves the range; want an error, got %s%d", p.Step, p.Octave)
	}
}

// An unreadable step is a real error, not something to guess at.
func TestUnknownStepIsAnError(t *testing.T) {
	const score = `<score-partwise><pitch><step>H</step><octave>4</octave></pitch></score-partwise>`
	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err == nil {
		t.Error("expected an error for an unrecognized step")
	}
}

// OMR frequently fails to link the instrument name printed on the page to the
// staff, falling back to a placeholder. When the page itself names a violin,
// the placeholder is corrected rather than left to mislabel the part.
func TestGenericPartNameFixedFromCredit(t *testing.T) {
	const score = `<score-partwise>
<credit page="1"><credit-words>Violin</credit-words></credit>
<part-list><score-part id="P1">
<part-name>Voice</part-name>
<part-abbreviation>Voice</part-abbreviation>
<score-instrument id="P1-I1"><instrument-name>Voice Oohs</instrument-name></score-instrument>
<midi-instrument id="P1-I1"><midi-program>54</midi-program></midi-instrument>
</score-part></part-list></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	if strings.Contains(got, "Voice") {
		t.Errorf("placeholder part name survived:\n%s", got)
	}
	if !strings.Contains(got, "<part-name>Viola</part-name>") {
		t.Errorf("part should be named Viola:\n%s", got)
	}
	if !strings.Contains(got, "<midi-program>42</midi-program>") {
		t.Errorf("playback should use the viola patch:\n%s", got)
	}
}

// Without a credit naming a violin there is no evidence the part is one, so a
// placeholder is left exactly as found.
func TestGenericPartNameLeftAloneWithoutCredit(t *testing.T) {
	const score = `<score-partwise><part-list><score-part id="P1">
<part-name>Voice</part-name>
<midi-instrument id="P1-I1"><midi-program>54</midi-program></midi-instrument>
</score-part></part-list></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "<part-name>Voice</part-name>") {
		t.Errorf("part name should be untouched without evidence:\n%s", got)
	}
	if !strings.Contains(got, "<midi-program>54</midi-program>") {
		t.Errorf("playback patch should be untouched:\n%s", got)
	}
}

// A part with a real name that is not a violin is never renamed, even when a
// violin credit appears elsewhere on the page -- a score may hold several
// instruments and only the violin part becomes a viola part.
func TestNamedNonViolinPartNeverRenamed(t *testing.T) {
	const score = `<score-partwise>
<credit page="1"><credit-words>Violin and Piano</credit-words></credit>
<part-list><score-part id="P1"><part-name>Piano</part-name>
<midi-instrument id="P1-I1"><midi-program>1</midi-program></midi-instrument>
</score-part></part-list></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "<part-name>Piano</part-name>") {
		t.Errorf("a named non-violin part must not be renamed:\n%s", got)
	}
	if !strings.Contains(got, "<midi-program>1</midi-program>") {
		t.Errorf("a piano patch must not be reassigned:\n%s", got)
	}
}

// OMR deposits unidentified page text in credits, including measure numbers
// scraped off the left margin. Those print as stray lines in the header.
func TestScrapedMeasureNumbersDropped(t *testing.T) {
	const score = `<score-partwise>
<credit page="1"><credit-words>Daisy Bell</credit-words></credit>
<credit page="1"><credit-words>10</credit-words></credit>
<credit page="1"><credit-words>18</credit-words></credit>
<credit page="1"><credit-words>1901</credit-words></credit>
</score-partwise>`

	var out strings.Builder
	st, err := Transpose(strings.NewReader(score), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()

	if !strings.Contains(got, "Daisy Bell") {
		t.Errorf("real credits must survive:\n%s", got)
	}
	if strings.Contains(got, ">10<") || strings.Contains(got, ">18<") {
		t.Errorf("scraped measure numbers should be dropped:\n%s", got)
	}
	if st.DroppedCredits != 3 {
		t.Errorf("DroppedCredits = %d, want 3", st.DroppedCredits)
	}
	// The wrapper goes too, leaving no empty <credit> shells.
	if strings.Contains(got, "<credit page=\"1\"></credit>") {
		t.Errorf("empty credit wrapper left behind:\n%s", got)
	}
}

// Under --preserve-pitch the part is relabelled but playback is not touched,
// since the sounding pitches are unchanged.
func TestPreservePitchLeavesMidiProgram(t *testing.T) {
	const score = `<score-partwise>
<credit page="1"><credit-words>Violin</credit-words></credit>
<part-list><score-part id="P1"><part-name>Voice</part-name>
<midi-instrument id="P1-I1"><midi-program>54</midi-program></midi-instrument>
</score-part></part-list></score-partwise>`

	var out strings.Builder
	if _, err := Transpose(strings.NewReader(score), &out, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "<midi-program>54</midi-program>") {
		t.Errorf("preserve-pitch must not change playback:\n%s", out.String())
	}
}
