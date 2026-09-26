package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Transposing a violin part for viola is a *diatonic* operation, not a chromatic
// one. Violin strings E-A-D-G map onto viola A-D-G-C, so every letter name moves
// down four diatonic steps and the written shape of the music is preserved --
// fingerings and muscle memory carry over unchanged.
//
// Naively shifting "down 7 semitones" and respelling would be wrong: it destroys
// the key signature relationship and produces enharmonic spellings a player has
// to decode. Instead we move the letter and let the accidental ride along.
//
// The one exception is F. A perfect fifth below F is B-flat, not B-natural --
// F to B is a tritone (6 semitones). So F alone carries an alter delta of -1,
// which is what keeps every mapping below exactly 7 semitones.
type stepMapping struct {
	step        string // resulting diatonic letter
	octaveDelta int    // -1 when the interval crosses the C boundary downward
	alterDelta  int    // correction to keep the interval perfect (F -> B-flat)
}

// fifthDown is indexed by diatonic letter. Verified exhaustively: every entry
// lowers the sounding pitch by exactly 7 semitones across all octaves and all
// alterations from double-flat to double-sharp.
var fifthDown = map[string]stepMapping{
	"C": {"F", -1, 0},
	"D": {"G", -1, 0},
	"E": {"A", -1, 0},
	"F": {"B", -1, -1}, // tritone exception: F -> B-flat
	"G": {"C", 0, 0},
	"A": {"D", 0, 0},
	"B": {"E", 0, 0},
}

// semitoneOf gives the pitch class of each natural letter, used to verify the
// interval and to respell alterations that fall outside engravable range.
var semitoneOf = map[string]int{"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}

// enharmonic maps a pitch class to a sane default spelling, used only in the
// rare case where a transposed note would need a triple accidental.
var enharmonic = []struct {
	step  string
	alter int
}{
	{"C", 0}, {"C", 1}, {"D", 0}, {"E", -1}, {"E", 0}, {"F", 0},
	{"F", 1}, {"G", 0}, {"A", -1}, {"A", 0}, {"B", -1}, {"B", 0},
}

// Stats records what the engine actually touched, so the CLI can report real
// numbers instead of claiming success blindly.
type Stats struct {
	Clefs       int
	Pitches     int
	Respellings int
	KeyFifths   int
	PartNames   int
	Harmonies   int
	Accidentals int
	// DroppedCredits counts page text discarded as OMR noise.
	DroppedCredits int
	// CorrectedChords counts chords whose root was restored from the source
	// PDF's text layer after recognition misread it.
	CorrectedChords int
}

// pitch mirrors the MusicXML <pitch> element. Order matters on output: the
// MusicXML DTD requires step, then alter, then octave.
type pitch struct {
	Step   string `xml:"step"`
	Alter  *int   `xml:"alter,omitempty"`
	Octave int    `xml:"octave"`
}

// transposePitch applies the diatonic fifth-down mapping to a single pitch.
// It returns whether the result had to be respelled enharmonically.
func transposePitch(p *pitch) (bool, error) {
	m, ok := fifthDown[strings.ToUpper(strings.TrimSpace(p.Step))]
	if !ok {
		return false, fmt.Errorf("unrecognized diatonic step %q", p.Step)
	}

	alter := 0
	if p.Alter != nil {
		alter = *p.Alter
	}

	newStep := m.step
	newAlter := alter + m.alterDelta
	newOct := p.Octave + m.octaveDelta

	respelled := false
	// Triple accidentals are legal MusicXML but no engraver renders them
	// usefully, so fall back to an enharmonic spelling of the same sounding
	// pitch. In practice this only arises from an F-double-flat source.
	if newAlter < -2 || newAlter > 2 {
		abs := (newOct+1)*12 + semitoneOf[newStep] + newAlter
		if abs < 0 {
			return false, fmt.Errorf("transposition falls below MIDI range for %s%+d octave %d", p.Step, alter, p.Octave)
		}
		e := enharmonic[((abs%12)+12)%12]
		newStep, newAlter, newOct = e.step, e.alter, abs/12-1
		respelled = true
	}

	// MusicXML defines octaves 0-9. A transposition that leaves the range has
	// produced a pitch no renderer can engrave, which is better reported than
	// written out as silently invalid data.
	if newOct < 0 || newOct > 9 {
		return false, fmt.Errorf("transposing %s%s%d down a fifth leaves the engravable octave range (0-9)",
			p.Step, alterSuffix(alter), p.Octave)
	}

	p.Step = newStep
	p.Octave = newOct
	if newAlter == 0 {
		p.Alter = nil // omit a redundant <alter>0</alter>
	} else {
		v := newAlter
		p.Alter = &v
	}
	return respelled, nil
}

// alterSuffix renders an alteration for error messages.
func alterSuffix(alter int) string {
	switch {
	case alter > 0:
		return strings.Repeat("#", alter)
	case alter < 0:
		return strings.Repeat("b", -alter)
	}
	return ""
}

// Transpose streams MusicXML from r to w, rewriting clefs, pitches and key
// signatures as it goes. It decodes token by token rather than building a
// document tree, so memory stays flat regardless of score length -- a full
// symphony costs the same as a single page.
func Transpose(r io.Reader, w io.Writer, preservePitch bool) (Stats, error) {
	return TransposeWithCorrections(r, w, preservePitch, nil)
}

// TransposeWithCorrections is Transpose with a set of chord-root corrections
// derived from the source PDF's text layer, applied before transposition so the
// corrected root is what gets moved down a fifth.
func TransposeWithCorrections(r io.Reader, w io.Writer, preservePitch bool, corrections []chordCorrection) (Stats, error) {
	var st Stats
	// Per-call state keeps concurrent Transpose calls independent.
	state := &transposeState{corrections: indexCorrections(corrections)}

	dec := xml.NewDecoder(r)
	// Scores from OMR tools reference DTDs we neither have nor need, and may
	// carry non-UTF8 encodings. Resolve entities permissively and pass through
	// any charset unchanged.
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	defer enc.Flush()

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, fmt.Errorf("decoding MusicXML: %w", err)
		}
		tok = stripNamespace(tok)

		switch t := tok.(type) {
		case xml.ProcInst:
			// The caller writes the XML declaration and DOCTYPE, so drop the
			// source's own declaration rather than emitting a second one.
			if strings.EqualFold(t.Target, "xml") {
				continue
			}
		case xml.Directive:
			// Likewise skip the source DOCTYPE; ours is already in place and a
			// duplicate would make the document invalid.
			if bytes.HasPrefix(bytes.TrimSpace(t), []byte("DOCTYPE")) {
				continue
			}
		}

		if se, ok := tok.(xml.StartElement); ok {
			switch se.Name.Local {
			case "clef":
				n, err := rewriteClef(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.Clefs += n
				continue

			case "pitch":
				if preservePitch {
					break // fall through to the generic copy below
				}
				respelled, err := state.rewritePitch(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.Pitches++
				if respelled {
					st.Respellings++
				}
				continue

			case "accidental":
				// <accidental> is the printed symbol, a sibling of <pitch>.
				// Left alone it contradicts the transposed pitch: an F-sharp
				// mapped to B would still print a sharp and engrave as B-sharp,
				// sounding a semitone wrong.
				if preservePitch {
					break
				}
				n, err := state.rewriteAccidental(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.Accidentals += n
				continue

			case "print":
				// A new system resets the chord counter, which is how a
				// correction derived from the page is located in the stream.
				for _, a := range se.Attr {
					if a.Name.Local == "new-system" && a.Value == "yes" {
						if state.seenSystem {
							state.system++
						}
						state.chordIndex = 0
					}
				}
				state.seenSystem = true

			case "root-step", "bass-step":
				// Chord symbols must move with the music. Leaving them behind
				// would print D-major harmony over a part sounding in G.
				if preservePitch {
					break
				}
				n, err := state.rewriteRootStep(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.Harmonies += n
				continue

			case "root-alter", "bass-alter":
				if preservePitch {
					break
				}
				if err := state.rewriteRootAlter(dec, enc, &se); err != nil {
					return st, err
				}
				continue

			case "credit":
				// Buffer the whole wrapper so a credit whose only content was
				// OMR noise can be removed entirely, rather than leaving an
				// empty <credit> element behind.
				n, dropped, err := state.rewriteCreditBlock(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.PartNames += n
				st.DroppedCredits += dropped
				continue

			case "credit-words":
				// Credits carry the visible page text. They are also where OMR
				// tools deposit anything they could not identify, including
				// stray measure numbers scraped off the left margin.
				n, dropped, err := state.rewriteCredit(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.PartNames += n
				st.DroppedCredits += dropped
				continue

			case "midi-program":
				// A part relabelled as viola should also play back as one.
				// Audiveris guesses a voice patch when it cannot identify the
				// instrument, which sounds like a choir on playback.
				if preservePitch {
					break
				}
				n, err := state.rewriteMidiProgram(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.PartNames += n
				continue

			case "part-name", "part-abbreviation", "instrument-name":
				// A part still labeled "Violin" on a viola stand is a nuisance.
				n, err := state.rewritePartName(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.PartNames += n
				continue

			case "key":
				if preservePitch {
					break
				}
				n, err := rewriteKey(dec, enc, &se)
				if err != nil {
					return st, err
				}
				st.KeyFifths += n
				continue
			}
		}

		if err := enc.EncodeToken(tok); err != nil {
			return st, fmt.Errorf("encoding token: %w", err)
		}
	}

	if err := enc.Flush(); err != nil {
		return st, fmt.Errorf("flushing output: %w", err)
	}
	st.CorrectedChords = state.appliedCorrections
	return st, nil
}

// clef is decoded as a struct so we can rewrite sign/line while preserving any
// attributes (staff number, print-object) the source carried.
type clef struct {
	Sign       string `xml:"sign"`
	Line       *int   `xml:"line,omitempty"`
	ClefOctave *int   `xml:"clef-octave-change,omitempty"`
}

// rewriteClef converts treble (G/2) to alto (C/3), the viola's home clef.
// Clefs that are already alto, or that are percussion/tab/bass staves, are
// passed through untouched -- rewriting a bass clef would be wrong in a part
// that legitimately contains one.
func rewriteClef(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, error) {
	var c clef
	if err := dec.DecodeElement(&c, se); err != nil {
		return 0, fmt.Errorf("decoding <clef>: %w", err)
	}

	changed := 0
	if strings.EqualFold(c.Sign, "G") && c.Line != nil && *c.Line == 2 && c.ClefOctave == nil {
		line := 3
		c.Sign, c.Line = "C", &line
		changed = 1
	}

	if err := enc.EncodeElement(c, *se); err != nil {
		return 0, fmt.Errorf("encoding <clef>: %w", err)
	}
	return changed, nil
}

// rewritePitch decodes one <pitch>, transposes it, and re-emits it, recording
// the resulting alteration so a following <accidental> can be made to agree.
func (t *transposeState) rewritePitch(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (bool, error) {
	var p pitch
	if err := dec.DecodeElement(&p, se); err != nil {
		return false, fmt.Errorf("decoding <pitch>: %w", err)
	}

	respelled, err := transposePitch(&p)
	if err != nil {
		return false, err
	}

	t.lastAlter = 0
	if p.Alter != nil {
		t.lastAlter = *p.Alter
	}
	t.haveNote = true

	if err := enc.EncodeElement(p, *se); err != nil {
		return false, fmt.Errorf("encoding <pitch>: %w", err)
	}
	return respelled, nil
}

// accidentalNames maps an alteration to the MusicXML accidental symbol that
// prints it. These are the five that a diatonic fifth can produce.
var accidentalNames = map[int]string{
	-2: "flat-flat",
	-1: "flat",
	0:  "natural",
	1:  "sharp",
	2:  "double-sharp",
}

// courtesyAccidentals are the editorial variants; they carry the same meaning
// as the plain symbol, so a rewrite maps them onto it rather than inventing a
// spelling the renderer may not know.
var courtesyAccidentals = map[string]bool{
	"natural-sharp": true, "natural-flat": true,
	"sharp-sharp": true, "flat-flat": true,
}

// rewriteAccidental brings the printed accidental into agreement with the
// transposed pitch. The symbol is what the engraver draws, so leaving a stale
// "sharp" beside a transposed B would print B-sharp and sound a semitone high.
func (t *transposeState) rewriteAccidental(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, error) {
	var symbol string
	if err := dec.DecodeElement(&symbol, se); err != nil {
		return 0, fmt.Errorf("decoding <accidental>: %w", err)
	}
	trimmed := strings.TrimSpace(symbol)

	// Without a preceding pitch there is nothing to reconcile against, and an
	// unfamiliar symbol (quarter tones, editorial brackets) is left to the
	// renderer rather than guessed at.
	_, known := accidentalNames[t.lastAlter]
	if !t.haveNote || !known || !isStandardAccidental(trimmed) {
		if err := enc.EncodeElement(symbol, *se); err != nil {
			return 0, fmt.Errorf("encoding <accidental>: %w", err)
		}
		return 0, nil
	}

	want := accidentalNames[t.lastAlter]
	t.haveNote = false // one accidental belongs to one note

	if want == trimmed {
		if err := enc.EncodeElement(symbol, *se); err != nil {
			return 0, fmt.Errorf("encoding <accidental>: %w", err)
		}
		return 0, nil
	}

	if err := enc.EncodeElement(want, *se); err != nil {
		return 0, fmt.Errorf("encoding <accidental>: %w", err)
	}
	return 1, nil
}

// isStandardAccidental reports whether a symbol is one this engine understands
// well enough to replace.
func isStandardAccidental(s string) bool {
	switch s {
	case "sharp", "flat", "natural", "double-sharp", "double-flat", "flat-flat", "sharp-sharp":
		return true
	}
	return courtesyAccidentals[s]
}

// stripNamespace clears the resolved namespace that encoding/xml attaches to
// every parsed name. Re-encoding a token that carries both a resolved Space and
// the original xmlns attribute emits the declaration twice, producing malformed
// XML that strict parsers reject.
func stripNamespace(tok xml.Token) xml.Token {
	switch t := tok.(type) {
	case xml.StartElement:
		t.Name.Space = ""
		attrs := t.Attr[:0]
		for _, a := range t.Attr {
			// Keep xmlns declarations exactly as written, drop the resolved
			// duplicates the decoder synthesizes.
			if a.Name.Space == "xmlns" || a.Name.Local == "xmlns" {
				if a.Name.Space == "xmlns" {
					a.Name.Space = "xmlns"
				}
				attrs = append(attrs, a)
				continue
			}
			a.Name.Space = ""
			attrs = append(attrs, a)
		}
		t.Attr = attrs
		return t
	case xml.EndElement:
		t.Name.Space = ""
		return t
	}
	return tok
}

// key mirrors <key>. KeyOctave is captured as raw XML so that <key-octave>
// elements survive the round trip; decoding into a struct without a field for
// them would silently drop them from the score.
type key struct {
	Cancel    *int     `xml:"cancel,omitempty"`
	Fifths    *int     `xml:"fifths,omitempty"`
	Mode      string   `xml:"mode,omitempty"`
	KeyOctave []rawXML `xml:"key-octave,omitempty"`
}

// rawXML preserves an element unchanged, attributes and all.
type rawXML struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Inner   []byte     `xml:",innerxml"`
}

// rewriteKey shifts the key signature one step flatward on the circle of
// fifths, which is what a descending perfect fifth does. C major becomes F
// major, G major becomes C, and so on.
//
// Only the flat side can overflow: going down a fifth decreases the count, so
// -7 (seven flats) would become an unwritable eight flats. That case wraps
// enharmonically to +4 (four sharps), the same sounding key. The sharp side
// never exceeds +7 here because the value only ever decreases.
func rewriteKey(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, error) {
	var k key
	if err := dec.DecodeElement(&k, se); err != nil {
		return 0, fmt.Errorf("decoding <key>: %w", err)
	}

	changed := 0
	if k.Fifths != nil {
		// A key signature outside [-7, +7] is not a real signature; pass such a
		// value through untouched rather than compounding the error.
		if *k.Fifths >= -7 && *k.Fifths <= 7 {
			f := *k.Fifths - 1
			if f < -7 {
				f += 12 // eight flats is unwritable; respell as four sharps
			}
			k.Fifths = &f
			changed = 1
		}
	}

	if err := enc.EncodeElement(k, *se); err != nil {
		return 0, fmt.Errorf("encoding <key>: %w", err)
	}
	return changed, nil
}

// transposeState carries the little context the streaming rewriter needs
// between adjacent tokens. MusicXML orders <root-step> before <root-alter>, so
// the step's tritone correction is recorded here for the alter that follows.
type transposeState struct {
	pendingAlter int
	// sawViolinCredit records that a credit on the page names a violin. OMR
	// tools frequently fail to link that text to the staff and fall back to a
	// generic part name, so the credit is the only surviving evidence of what
	// the part actually is.
	sawViolinCredit bool
	// renamedPart records that this part was identified as a violin part and
	// relabelled, which is the precondition for touching its playback patch.
	renamedPart bool

	// corrections maps a chord's position in reading order to the root the
	// source page actually shows, keyed by system and index within it.
	corrections map[[2]int]chordCorrection
	// system and chordIndex track position while streaming, so each harmony can
	// be matched against the corrections computed from the PDF.
	system     int
	chordIndex int
	seenSystem bool
	// correctedChord holds a correction awaiting its <root-alter>, so the
	// alteration printed on the page replaces the recognized one.
	correctedChord *chordCorrection
	// appliedCorrections counts corrections actually reached in the stream.
	appliedCorrections int
	// lastAlter is the alteration of the most recently transposed <pitch>,
	// used to rewrite the <accidental> that follows it within the same <note>.
	lastAlter int
	// haveNote reports whether a pitch has been seen, so a stray <accidental>
	// outside any note is passed through rather than rewritten from stale data.
	haveNote bool
}

// rewriteRootStep transposes a chord symbol's root (or bass) letter down a
// fifth, using the identical mapping applied to sounding pitches.
func (t *transposeState) rewriteRootStep(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, error) {
	var step string
	if err := dec.DecodeElement(&step, se); err != nil {
		return 0, fmt.Errorf("decoding <%s>: %w", se.Name.Local, err)
	}

	key := strings.ToUpper(strings.TrimSpace(step))

	// A root the source page disagrees with is corrected before transposition,
	// so the fifth is taken from the chord that was actually printed. Only
	// <root-step> is counted and corrected; <bass-step> rides along with it.
	if se.Name.Local == "root-step" {
		pos := [2]int{t.system, t.chordIndex}
		t.chordIndex++
		if c, found := t.corrections[pos]; found {
			key = c.step
			t.correctedChord = &c
			t.appliedCorrections++
		}
	}

	m, ok := fifthDown[key]
	if !ok {
		t.pendingAlter = 0
		if err := enc.EncodeElement(step, *se); err != nil {
			return 0, fmt.Errorf("encoding <%s>: %w", se.Name.Local, err)
		}
		return 0, nil
	}

	t.pendingAlter = m.alterDelta
	if err := enc.EncodeElement(m.step, *se); err != nil {
		return 0, fmt.Errorf("encoding <%s>: %w", se.Name.Local, err)
	}
	return 1, nil
}

// rewriteRootAlter applies the correction the preceding root-step recorded, so
// that (for example) an F chord becomes B-flat rather than B.
func (t *transposeState) rewriteRootAlter(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) error {
	var alter int
	if err := dec.DecodeElement(&alter, se); err != nil {
		return fmt.Errorf("decoding <%s>: %w", se.Name.Local, err)
	}
	// Take the alteration from the page too, not just the letter.
	if t.correctedChord != nil && se.Name.Local == "root-alter" {
		alter = t.correctedChord.alter
		t.correctedChord = nil
	}
	if err := enc.EncodeElement(alter+t.pendingAlter, *se); err != nil {
		return fmt.Errorf("encoding <%s>: %w", se.Name.Local, err)
	}
	t.pendingAlter = 0
	return nil
}

// indexCorrections keys corrections by their position in reading order so the
// streaming rewriter can find each one in constant time.
func indexCorrections(cs []chordCorrection) map[[2]int]chordCorrection {
	if len(cs) == 0 {
		return nil
	}
	m := make(map[[2]int]chordCorrection, len(cs))
	for _, c := range cs {
		m[[2]int{c.system, c.index}] = c
	}
	return m
}

// violinNaming matches the instrument word in a part label, in the common
// casings and the abbreviated forms OMR tools emit.
var violinNaming = strings.NewReplacer(
	"Violin", "Viola",
	"violin", "viola",
	"VIOLIN", "VIOLA",
	"Vln", "Vla",
	"vln", "vla",
	"Vn.", "Va.",
	"Vn", "Va",
)

// genericPartNames are the placeholders OMR tools assign when they cannot read
// a part's real label. Audiveris defaults to "Voice"; others use similar stand-in
// names. A placeholder carries no information, so it is safe to replace when the
// page itself says what the instrument is.
var genericPartNames = map[string]bool{
	"voice":      true,
	"voice oohs": true,
	"part":       true,
	"unnamed":    true,
	"instrument": true,
	"":           true,
}

// rewritePartName relabels a violin part as viola.
//
// Two cases are handled. The straightforward one is a label that actually says
// "Violin". The second arises because OMR frequently fails to link the
// instrument name printed at the top of the page to the staff beneath it, and
// falls back to a placeholder such as "Voice". When that happens and a credit
// on the page does name a violin, the placeholder is replaced rather than left
// to mislabel the part -- the evidence is already in the document.
//
// A part with a real name that is not a violin is never touched: relabelling
// every part would be wrong on a score that contains more than one instrument.
func (t *transposeState) rewritePartName(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, error) {
	var name string
	if err := dec.DecodeElement(&name, se); err != nil {
		return 0, fmt.Errorf("decoding <%s>: %w", se.Name.Local, err)
	}

	changed := 0
	switch {
	case violinNaming.Replace(name) != name:
		name = violinNaming.Replace(name)
		changed = 1

	case t.sawViolinCredit && genericPartNames[strings.ToLower(strings.TrimSpace(name))]:
		// The recognizer gave up on the label; the page did not.
		name = "Viola"
		changed = 1
	}
	if changed == 1 {
		t.renamedPart = true
	}

	if err := enc.EncodeElement(name, *se); err != nil {
		return 0, fmt.Errorf("encoding <%s>: %w", se.Name.Local, err)
	}
	return changed, nil
}

// rewriteCreditBlock processes an entire <credit> element. The wrapper is only
// emitted if some visible text survives, so a credit that held nothing but a
// scraped measure number disappears cleanly instead of leaving an empty shell.
func (t *transposeState) rewriteCreditBlock(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, int, error) {
	// Collected tokens are replayed only if some visible text survives.
	var pending []xml.Token
	renamed, dropped, kept := 0, 0, 0
	depth := 0

	for {
		tok, err := dec.Token()
		if err != nil {
			return 0, 0, fmt.Errorf("reading <credit>: %w", err)
		}
		tok = stripNamespace(tok)

		if s, ok := tok.(xml.StartElement); ok && s.Name.Local == "credit-words" {
			var text string
			if err := dec.DecodeElement(&text, &s); err != nil {
				return 0, 0, fmt.Errorf("decoding <credit-words>: %w", err)
			}
			keep, n := t.creditText(&text)
			if !keep {
				dropped++
				continue
			}
			kept++
			renamed += n
			pending = append(pending, s, xml.CharData(text), s.End())
			continue
		}

		if s, ok := tok.(xml.StartElement); ok {
			depth++
			pending = append(pending, s)
			continue
		}
		if e, ok := tok.(xml.EndElement); ok {
			if depth == 0 && e.Name.Local == se.Name.Local {
				break
			}
			depth--
			pending = append(pending, e)
			continue
		}
		pending = append(pending, xml.CopyToken(tok))
	}

	// Nothing visible left: drop the wrapper along with its contents.
	if kept == 0 {
		return renamed, dropped, nil
	}

	if err := enc.EncodeToken(*se); err != nil {
		return 0, 0, fmt.Errorf("encoding <credit>: %w", err)
	}
	for _, tok := range pending {
		if err := enc.EncodeToken(tok); err != nil {
			return 0, 0, fmt.Errorf("encoding <credit>: %w", err)
		}
	}
	if err := enc.EncodeToken(se.End()); err != nil {
		return 0, 0, fmt.Errorf("encoding <credit>: %w", err)
	}
	return renamed, dropped, nil
}

// violaMidiProgram is General MIDI program 42 (Viola), one-based as MusicXML
// writes it.
const violaMidiProgram = 42

// rewriteMidiProgram points playback at a viola patch, but only for a part this
// engine has already decided is a violin part. Without that check a percussion
// or piano staff in the same file would be silently reassigned.
func (t *transposeState) rewriteMidiProgram(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, error) {
	var program int
	if err := dec.DecodeElement(&program, se); err != nil {
		return 0, fmt.Errorf("decoding <midi-program>: %w", err)
	}

	changed := 0
	if t.renamedPart && program != violaMidiProgram {
		program = violaMidiProgram
		changed = 1
	}

	if err := enc.EncodeElement(program, *se); err != nil {
		return 0, fmt.Errorf("encoding <midi-program>: %w", err)
	}
	return changed, nil
}

// measureNumberCredit matches a credit consisting only of digits. Real credits
// are titles, composers, dedications or performance directions; a bare number is
// a measure number the recognizer scraped off the left margin and mistook for
// page text. Left in place it prints as a stray line in the header.
var measureNumberCredit = regexp.MustCompile(`^\d{1,4}$`)

// creditText decides the fate of one credit string, rewriting it in place. It
// reports whether the credit should be kept, and whether it was renamed.
func (t *transposeState) creditText(text *string) (keep bool, renamed int) {
	if measureNumberCredit.MatchString(strings.TrimSpace(*text)) {
		return false, 0
	}

	// Record that the page names a violin before rewriting the text, so a later
	// generic part name can be corrected. Credits precede the part list in a
	// MusicXML document, so this is always set in time.
	lower := strings.ToLower(*text)
	if strings.Contains(lower, "violin") || strings.Contains(lower, "vln") {
		t.sawViolinCredit = true
	}

	if s := violinNaming.Replace(*text); s != *text {
		*text = s
		return true, 1
	}
	return true, 0
}

// rewriteCredit handles a <credit-words> element encountered outside any
// <credit> wrapper, which is unusual but permitted.
func (t *transposeState) rewriteCredit(dec *xml.Decoder, enc *xml.Encoder, se *xml.StartElement) (int, int, error) {
	var text string
	if err := dec.DecodeElement(&text, se); err != nil {
		return 0, 0, fmt.Errorf("decoding <credit-words>: %w", err)
	}

	keep, renamed := t.creditText(&text)
	if !keep {
		return 0, 1, nil
	}

	if err := enc.EncodeElement(text, *se); err != nil {
		return 0, 0, fmt.Errorf("encoding <credit-words>: %w", err)
	}
	return renamed, 0, nil
}

// String renders the stats as the one-line summary the CLI prints on success.
func (s Stats) String() string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(s.Clefs))
	b.WriteString(" clef(s) -> alto, ")
	b.WriteString(strconv.Itoa(s.Pitches))
	b.WriteString(" pitch(es) down a fifth, ")
	b.WriteString(strconv.Itoa(s.KeyFifths))
	b.WriteString(" key signature(s) adjusted")
	if s.Harmonies > 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(s.Harmonies))
		b.WriteString(" chord symbol(s) transposed")
	}
	if s.CorrectedChords > 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(s.CorrectedChords))
		b.WriteString(" chord(s) corrected from the source PDF")
	}
	if s.PartNames > 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(s.PartNames))
		b.WriteString(" part label(s) renamed")
	}
	if s.Respellings > 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(s.Respellings))
		b.WriteString(" respelled enharmonically")
	}
	return b.String()
}
