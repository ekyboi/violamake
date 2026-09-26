# violamake

Turn violin sheet music into a viola part.

```
cd ~/scores
violamake sonata.pdf        # writes sonata-viola.pdf
```

`violamake` reads a score, rewrites the treble clef as alto clef, transposes
everything down a perfect fifth, and writes a new PDF beside the original. It
runs entirely on your machine — nothing is uploaded.

## Why a fifth

The violin is tuned E–A–D–G. The viola is tuned A–D–G–C. That is the same set
of intervals, moved down a perfect fifth:

| Violin string | E | A | D | G |
|---|---|---|---|---|
| Viola string  | A | D | G | C |

So a note played on the violin's second string lands on the viola's second
string, at the same finger, in the same position. Transposing down a fifth and
switching to alto clef leaves the *written* notes in the same place on the
staff. A violinist reading the output uses the fingerings they already know.

This is why the transposition is diatonic rather than chromatic. "Down seven
semitones" would give the right pitches but wreck the spelling: sharps would
turn into unrelated flats and the key signature would stop matching the notes.
Instead each letter moves down four diatonic steps and keeps its accidental.

There is exactly one exception, and it is the interesting part of the program.
A fifth below F is **B♭**, not B — F to B is a tritone, six semitones, not
seven. So F is the only letter that also shifts its accidental:

| From | C | D | E | F | G | A | B |
|---|---|---|---|---|---|---|---|
| To | F | G | A | **B♭** | C | D | E |

The mapping is verified exhaustively in the tests: every letter, every
alteration from double-flat to double-sharp, every octave, is confirmed to fall
by exactly seven semitones.

## Install

You need Go (the module targets 1.27) to build, plus two external tools.

```sh
go install github.com/ekyboi/violamake@latest
```

Or build from a clone:

```sh
git clone https://github.com/ekyboi/violamake.git && cd violamake
go build -o violamake .
```

The only Go dependency is [`ledongthuc/pdf`][pdflib], used to read chord symbols
from a PDF's text layer. It has no dependencies of its own.

[pdflib]: https://github.com/ledongthuc/pdf

**Recognition (only needed for PDF input)** — [Audiveris][av], an optical music
recognition engine. Download the installer for your platform and, on macOS, drag
`Audiveris.app` into `/Applications`; `violamake` finds it there without any
`$PATH` setup.

MuseScore is *not* an alternative. Its PDF import is a hosted service, not a
command-line feature, so it cannot do recognition offline.

**Engraving** — either of:

```sh
brew install verovio librsvg   # Verovio renders SVG; rsvg-convert makes the PDF
brew install lilypond          # LilyPond renders PDF directly
```

On Debian/Ubuntu: `sudo apt install verovio librsvg2-bin`, or `lilypond`.

**Text recognition (optional but recommended).** Without Tesseract language
data, Audiveris silently drops the title, composer and every chord symbol.
Audiveris needs the *legacy-capable* models — the LSTM-only set that Homebrew's
`tesseract` installs will not work:

```sh
mkdir -p ~/.local/share/tessdata
curl -L -o ~/.local/share/tessdata/eng.traineddata \
  https://github.com/tesseract-ocr/tessdata/raw/main/eng.traineddata
```

`violamake` looks there automatically. Set `TESSDATA_PREFIX` to override.

If anything is missing, `violamake` says so on startup and prints the exact
install command — it never fails halfway through a slow conversion.

[av]: https://github.com/Audiveris/audiveris/releases

## Usage

```
violamake [flags] <score.pdf>
```

The input is positional, so the common case is just `violamake score.pdf`. The
output is named after the input (`sonata.pdf` → `sonata-viola.pdf`) and written
to the same directory. Running it twice does not produce `-viola-viola`.

| Flag | Effect |
|---|---|
| `-preserve-pitch` | Swap the clef to alto but leave the pitches alone. For reading a violin part on viola at written pitch. |
| `-o <path>` | Write somewhere other than the default. |
| `-timeout <dur>` | Cap the whole run. Default `10m`. |
| `-v` | Show each stage and the output of every tool it calls. |

Accepted inputs: `.pdf`, `.musicxml`, `.xml`, `.mxl`. Giving it MusicXML skips
recognition entirely — much faster, and it does not need Audiveris installed.

## What gets transposed

Getting the notes right is not sufficient; a part is only usable if everything
on the page agrees with everything else.

- **Pitches** — down a diatonic fifth, accidentals preserved, octave decremented
  across the C boundary.
- **Clefs** — treble (G, line 2) becomes alto (C, line 3). Bass, percussion and
  already-alto clefs are left alone, since a part may legitimately contain one.
- **Key signature** — one step flatward on the circle of fifths. D major becomes
  G major. Without this the printed accidentals would contradict the notes.
- **Printed accidentals** — `<accidental>` is the symbol the engraver draws, and
  it is stored separately from the pitch. A stale `sharp` next to a transposed B
  engraves B♯ and sounds a semitone wrong, so it is rewritten to match.
- **Chord symbols** — root and bass move with the music, F♯ exception included.
  A part sounding in G major that still prints D-major chord symbols is useless
  to anyone comping from it.
- **Misread chord roots** — recognition reads chord symbols with OCR, which
  confuses letters of similar shape: a printed `Bm` comes back as `Em`. The
  mistake is undetectable downstream, because a wrong chord is still a valid
  chord. When the source PDF was engraved it carries a text layer holding those
  symbols exactly, so `violamake` reads them from the page and restores any root
  that disagrees, before transposing. See *Chord correction* below.
- **Part names** — "Violin" becomes "Viola", including the `Vln.`/`Vn.`
  abbreviations, in part names, instrument names and credits.
- **Placeholder part names** — recognition often fails to link the instrument
  name printed on the page to the staff below it, leaving a generic label such
  as "Voice". When a credit on the page names a violin, that placeholder is
  corrected and the playback patch is set to viola. A part with a real name that
  is not a violin is never touched, so a piano staff in the same file is safe.
- **Scraped page text** — recognition sometimes mistakes measure numbers in the
  left margin for page credits, which then print as stray lines in the header.
  Credits consisting only of digits are dropped.

Under `-preserve-pitch`, only the clef and part name change.

## How it works

Three stages, orchestrated with `os/exec` under a context deadline:

```
score.pdf ──> Audiveris ──> MusicXML ──> transposer ──> MusicXML ──> Verovio ──> score-viola.pdf
                (OMR)                    (native Go)                 (engraving)
```

The middle stage is the only part that is `violamake`'s own code. It streams the
document with `encoding/xml` token by token rather than building a tree, so
memory use is flat regardless of score length — a symphony costs the same as a
single page.

Everything intermediate lives in a private temp directory, never beside your
sheet music. On `SIGINT` or `SIGTERM` the context is cancelled, which kills the
child process group (Audiveris spawns a JVM that would otherwise survive), and
the temp directory is removed before exit. A second interrupt exits immediately,
still cleaning up first.

`violamake` will not overwrite your input, even when `-o` names it by a
different path, a symlink or a different letter case. If a run fails, a file
that was already at the output path is left exactly as it was found.

## Chord correction

Optical recognition reads chord symbols as images and runs OCR over them. On a
short string in a serif face that is unreliable: in the sample score a printed
`Bm` came back as `Em`. Nothing downstream can catch this, since `Em` is a
perfectly valid chord — it just is not the one on the page.

An engraved PDF, though, stores its text as character codes, not shapes. Those
codes are exact. So when the input is a PDF, `violamake` reads the chord symbols
from the text layer and compares them against what recognition produced.

The two lists are *aligned* rather than compared position by position, because
recognition regularly drops chords: in the sample it found 14 of the 17 printed.
A positional comparison would shift at the first omission and then rewrite every
chord after it. An alignment tolerates gaps on either side, so only genuine
disagreements are corrected and the chords recognition got right are untouched.

Only the root is corrected, and only when the two sources disagree about it.
Chord quality (`m`, `7`, `dim`) is left as recognition read it. A chord that is
missing from the recognized score is left missing, since placing it would mean
guessing its measure and beat.

If the PDF has no text layer — any scan — there is nothing to compare against,
and the score is converted on recognition alone.

## Layout

| File | Contents |
|---|---|
| `main.go` | CLI parsing, output naming, signal handling, lifecycle |
| `pipeline.go` | Dependency checking, subprocess orchestration, renderer quirks |
| `transposer.go` | The streaming XML transposition engine |
| `mxl.go` | Unwraps compressed `.mxl` archives |
| `chords.go` | Reads chord symbols from the PDF text layer and reconciles them |
| `proc_unix.go` / `proc_other.go` | Process-group teardown, per platform |

## Tests

```sh
go test ./...          # 34 tests
go test -race ./...
```

The suite covers the exhaustive interval check, the open-string mapping, the
F→B♭ tritone exception at both pitch and chord level, clef and key rewriting,
accidental reconciliation, namespace handling, and the edge cases that produce
invalid MusicXML (out-of-range octaves, nonsense key signatures, unknown steps).

## Known limitations

These come from the recognition stage, not the transposer.

- **OMR is imperfect on dense scores.** Audiveris may misread chord-symbol text
  as articulations, and it does not always get rhythms right on complex
  notation. Check the output before rehearsal — and keep the intermediate
  MusicXML (`-v` shows its path) if you want to correct it by hand.
- **Chord correction needs a text layer.** Misread chord roots are corrected
  automatically from the source PDF (see below), but only when that PDF was
  engraved rather than scanned. A scan has no text to compare against, so its
  chords are whatever recognition made of them — spot-check those.
- **Chords recognition misses entirely are not invented.** If a chord is absent
  from the recognized score, it stays absent. Restoring it would mean guessing
  which measure and beat it belongs to.
- **Only the LilyPond path is unverified.** The Verovio path is tested end to
  end; the LilyPond branch is written against its documented interface but has
  not been run against a live binary.
