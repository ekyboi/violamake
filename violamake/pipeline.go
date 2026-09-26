package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tool describes one external binary the pipeline depends on, along with the
// platform-appropriate way to install it. Being specific here is the difference
// between a user fixing their setup in ten seconds and filing an issue.
type Tool struct {
	Names   []string // acceptable binary names, tried in order
	Purpose string
	Install string
	// Fallbacks are absolute paths checked when nothing on $PATH matches.
	// GUI applications install as macOS bundles whose executables are not
	// linked into $PATH, but which run headless perfectly well.
	Fallbacks []string
	resolved  string
}

// Path returns the absolute path to the resolved binary.
func (t *Tool) Path() string { return t.resolved }

// omrTool converts a PDF into MusicXML. Audiveris is the only real local
// option: MuseScore's PDF import is a hosted service, not a CLI feature, so it
// cannot perform recognition offline and is deliberately not listed here.
func omrTool() *Tool {
	return &Tool{
		Names:   []string{"audiveris", "Audiveris"},
		Purpose: "optical music recognition (PDF -> MusicXML)",
		Install: "macOS:  download Audiveris from\n" +
			"              https://github.com/Audiveris/audiveris/releases\n" +
			"            and drag Audiveris.app into /Applications\n" +
			"    Linux:  install the .deb from that same releases page\n" +
			"    Note:   MuseScore is NOT an alternative here -- its PDF import is a\n" +
			"            cloud service, not a local CLI, so it cannot do this offline.",
		Fallbacks: []string{
			"/Applications/Audiveris.app/Contents/MacOS/Audiveris",
			os.ExpandEnv("$HOME/Applications/Audiveris.app/Contents/MacOS/Audiveris"),
		},
	}
}

// rendererTool engraves the transposed MusicXML back into a PDF.
func rendererTool() *Tool {
	return &Tool{
		Names:   []string{"verovio", "lilypond", "mscore", "musescore", "MuseScore"},
		Purpose: "score rendering (MusicXML -> PDF)",
		Install: "macOS:  brew install verovio librsvg   (or: brew install lilypond)\n" +
			"    Linux:  sudo apt install verovio librsvg2-bin\n" +
			"    Verovio engraves to SVG, so rsvg-convert is needed for PDF output;\n" +
			"    LilyPond renders PDF directly and needs no converter.",
	}
}

// CheckDependencies resolves every required tool against $PATH up front, so the
// run fails in the first second rather than after a slow OMR pass. All missing
// tools are reported together -- telling a user about one missing binary at a
// time is a miserable way to set up software.
func CheckDependencies(tools ...*Tool) error {
	var missing []string

	for _, t := range tools {
		found := false
		for _, name := range t.Names {
			if p, err := exec.LookPath(name); err == nil {
				t.resolved = p
				found = true
				break
			}
		}
		// Fall back to known application-bundle locations before giving up.
		for _, fb := range t.Fallbacks {
			if found {
				break
			}
			if info, err := os.Stat(fb); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				t.resolved = fb
				found = true
			}
		}
		if !found {
			missing = append(missing, fmt.Sprintf(
				"  missing: %s\n    tried:  %s\n    install:\n    %s",
				t.Purpose, strings.Join(t.Names, ", "), t.Install))
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("required external tools were not found on your $PATH:\n\n%s",
			strings.Join(missing, "\n\n"))
	}
	return nil
}

// Pipeline holds the resolved configuration for a single conversion run.
type Pipeline struct {
	Input         string
	Output        string
	PreservePitch bool
	Timeout       time.Duration
	Verbose       bool

	omr      *Tool
	renderer *Tool
	needsOMR bool

	// cleanupOnce guards workDir, which the signal handler and the normal
	// deferred path both race to remove.
	cleanupOnce sync.Once
	workDir     string
}

// NewPipeline resolves dependencies and prepares an isolated working directory
// for intermediate artifacts.
func NewPipeline(input, output string, preservePitch bool, timeout time.Duration, verbose bool) (*Pipeline, error) {
	// A MusicXML input is already recognized, so the OMR stage -- and its
	// dependency -- is only required for PDFs. Demanding an OMR tool in order
	// to transpose a file that needs no recognition would refuse work the tool
	// can plainly do.
	needsOMR := strings.EqualFold(filepath.Ext(input), ".pdf")

	omr, renderer := omrTool(), rendererTool()
	required := []*Tool{renderer}
	if needsOMR {
		required = append([]*Tool{omr}, required...)
	}
	if err := CheckDependencies(required...); err != nil {
		return nil, err
	}

	// Fail now rather than after a slow recognition pass if the destination
	// cannot be written.
	outDir := filepath.Dir(output)
	if info, err := os.Stat(outDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("output directory %s does not exist", outDir)
	}

	// Intermediates live in the system temp area, never beside the user's
	// sheet music -- a crashed run should not litter their score folder.
	workDir, err := os.MkdirTemp("", "violamake-")
	if err != nil {
		return nil, fmt.Errorf("creating temporary work directory: %w", err)
	}

	return &Pipeline{
		Input:         input,
		Output:        output,
		PreservePitch: preservePitch,
		Timeout:       timeout,
		Verbose:       verbose,
		omr:           omr,
		renderer:      renderer,
		workDir:       workDir,
		needsOMR:      needsOMR,
	}, nil
}

// Cleanup removes every intermediate artifact. It is safe to call more than
// once and is invoked both on the normal path and from the signal handler.
func (p *Pipeline) Cleanup() {
	p.cleanupOnce.Do(func() {
		if p.workDir == "" {
			return
		}
		if err := os.RemoveAll(p.workDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: could not remove temporary directory %s: %v\n", p.workDir, err)
		}
	})
}

// Run executes the three-stage pipeline. The context governs the whole run, so
// a Ctrl-C or a timeout kills any in-flight child process immediately.
func (p *Pipeline) Run(ctx context.Context) (Stats, error) {
	var st Stats

	rawXML := p.Input
	if isCompressed(rawXML) {
		extracted, err := extractMXL(rawXML, p.workDir)
		if err != nil {
			return st, err
		}
		rawXML = extracted
		p.logf("unwrapped compressed MusicXML")
	}
	if p.needsOMR {
		rawXML = filepath.Join(p.workDir, "extracted.musicxml")
		if err := p.runOMR(ctx, rawXML); err != nil {
			return st, err
		}
	} else {
		p.logf("input is already MusicXML; skipping recognition")
	}

	transposedXML := filepath.Join(p.workDir, "transposed.musicxml")
	// When the source is an engraved PDF it carries a text layer holding the
	// chord symbols exactly as printed. Recognition reads those with OCR and
	// can misread them, so the page's own text is used to correct the roots.
	var corrections []chordCorrection
	if p.needsOMR {
		corrections = p.chordCorrections(rawXML)
	}

	st, err := p.runTranspose(rawXML, transposedXML, corrections)
	if err != nil {
		return st, err
	}

	if err := p.runRender(ctx, transposedXML); err != nil {
		return st, err
	}
	return st, nil
}

// runOMR invokes the recognition tool. MuseScore and Audiveris take different
// arguments, so dispatch on the resolved binary name.
func (p *Pipeline) runOMR(ctx context.Context, outXML string) error {
	bin := p.omr.Path()
	base := strings.ToLower(filepath.Base(bin))

	var args []string
	switch {
	case strings.Contains(base, "audiveris"):
		// Audiveris batch mode writes into an output directory.
		args = []string{"-batch", "-export", "-output", p.workDir, "--", p.Input}
	default:
		// MuseScore converts by inferring formats from file extensions.
		args = []string{"-o", outXML, p.Input}
	}

	p.logf("running OMR: %s %s", bin, strings.Join(args, " "))
	if err := p.exec(ctx, bin, args...); err != nil {
		return fmt.Errorf("optical music recognition failed: %w", err)
	}

	// Audiveris names its export after the input file and writes the compressed
	// .mxl form by default, so locate it and normalize to plain MusicXML.
	if strings.Contains(base, "audiveris") {
		found, err := findExport(p.workDir)
		if err != nil {
			return err
		}
		if isCompressed(found) {
			extracted, err := extractMXL(found, p.workDir)
			if err != nil {
				return err
			}
			found = extracted
		}
		if found != outXML {
			if err := os.Rename(found, outXML); err != nil {
				return fmt.Errorf("locating Audiveris export: %w", err)
			}
		}
	}

	info, err := os.Stat(outXML)
	if err != nil {
		return fmt.Errorf("OMR tool reported success but produced no MusicXML at %s: %w", outXML, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("OMR tool produced an empty MusicXML file -- the PDF may be a scan that %s could not read", filepath.Base(bin))
	}
	return nil
}

// findExport locates a MusicXML file produced by a tool that chose its own name.
func findExport(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading work directory: %w", err)
	}
	// Preference order matters: Audiveris drops a .omr book file and a log
	// beside the export, and .xml is also the extension of its internal sheet
	// data, so take the unambiguous score formats first.
	for _, want := range []string{".mxl", ".musicxml", ".xml"} {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.EqualFold(filepath.Ext(e.Name()), want) {
				return filepath.Join(dir, e.Name()), nil
			}
		}
	}
	// Nothing usable came back. The most common cause is an interrupted earlier
	// run leaving a partly written book in the OMR tool's own cache, which it
	// then reloads instead of re-reading the PDF.
	var saw []string
	for _, e := range entries {
		saw = append(saw, e.Name())
	}
	detail := "the output directory is empty"
	if len(saw) > 0 {
		detail = "it wrote only: " + strings.Join(saw, ", ")
	}
	return "", fmt.Errorf("the OMR tool produced no MusicXML export -- %s\n"+
		"       if a previous run was interrupted, its cached book may be corrupt; clear it with:\n"+
		"         rm -rf ~/Library/Application\\ Support/AudiverisLtd", detail)
}

// runTranspose streams the recognized score through the native transformation.
// Files are streamed rather than buffered so memory stays flat on large scores.
func (p *Pipeline) runTranspose(inXML, outXML string, corrections []chordCorrection) (Stats, error) {
	var st Stats

	in, err := os.Open(inXML)
	if err != nil {
		return st, fmt.Errorf("opening recognized score: %w", err)
	}
	defer in.Close()

	out, err := os.Create(outXML)
	if err != nil {
		return st, fmt.Errorf("creating transposed score: %w", err)
	}
	defer out.Close()

	if _, err := out.WriteString(xmlHeader); err != nil {
		return st, fmt.Errorf("writing MusicXML header: %w", err)
	}

	st, err = TransposeWithCorrections(in, out, p.PreservePitch, corrections)
	if err != nil {
		return st, err
	}
	if err := out.Sync(); err != nil {
		return st, fmt.Errorf("flushing transposed score: %w", err)
	}

	p.logf("transposed: %s", st)
	return st, nil
}

// chordCorrections compares the chord symbols recognition produced against the
// text layer of the source PDF and returns the roots that need restoring.
//
// Every failure here is non-fatal: a scanned PDF has no text layer, and a PDF
// this library cannot parse is no reason to abandon a conversion that is
// otherwise fine. In both cases the score proceeds on recognition alone.
func (p *Pipeline) chordCorrections(recognizedXML string) []chordCorrection {
	fromPDF, err := chordsBySystem(p.Input)
	if err != nil {
		p.logf("could not read the PDF text layer (%v); using recognized chords as-is", err)
		return nil
	}
	if len(fromPDF) == 0 {
		p.logf("no chord symbols in the PDF text layer; the source is probably a scan")
		return nil
	}

	fromOMR, err := chordsFromMusicXML(recognizedXML)
	if err != nil {
		p.logf("could not read recognized chords (%v); skipping correction", err)
		return nil
	}

	corrections := reconcileChords(fromPDF, fromOMR)
	for _, c := range corrections {
		p.logf("chord corrected from the page: %s -> %s", c.from, c.to)
	}
	return corrections
}

// xmlHeader restores the declaration and DOCTYPE that encoding/xml does not
// emit. Renderers rely on the DOCTYPE to select the MusicXML parser.
const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE score-partwise PUBLIC "-//Recordare//DTD MusicXML 4.0 Partwise//EN" "http://www.musicxml.org/dtds/partwise.dtd">
`

// runRender engraves the transposed MusicXML into the final PDF.
func (p *Pipeline) runRender(ctx context.Context, inXML string) error {
	bin := p.renderer.Path()
	base := strings.ToLower(filepath.Base(bin))

	var args []string
	switch {
	case strings.Contains(base, "verovio"):
		// Verovio engraves to SVG, not PDF -- it has no PDF backend at all.
		// Render each page to SVG, then convert to a single PDF.
		return p.renderViaVerovio(ctx, bin, inXML)
	case strings.Contains(base, "lilypond"):
		// LilyPond cannot read MusicXML directly; convert first.
		lyFile, err := p.convertToLily(ctx, inXML)
		if err != nil {
			return err
		}
		// LilyPond appends its own .pdf extension to the -o basename.
		args = []string{"--pdf", "-o", strings.TrimSuffix(p.Output, filepath.Ext(p.Output)), lyFile}
	default:
		args = []string{"-o", p.Output, inXML}
	}

	p.logf("rendering: %s %s", bin, strings.Join(args, " "))
	if err := p.exec(ctx, bin, args...); err != nil {
		return fmt.Errorf("rendering the viola score failed: %w", err)
	}

	if _, err := os.Stat(p.Output); err != nil {
		return fmt.Errorf("renderer reported success but no PDF appeared at %s: %w", p.Output, err)
	}
	return nil
}

// renderViaVerovio engraves with Verovio and converts the resulting SVG pages
// into a single PDF. Verovio writes one SVG per page, naming them <base>_001.svg
// and so on when a score runs to multiple pages, or <base>.svg for a single one.
func (p *Pipeline) renderViaVerovio(ctx context.Context, bin, inXML string) error {
	svgBase := filepath.Join(p.workDir, "page")

	args := []string{"-f", "musicxml", "-t", "svg", "--all-pages", "-o", svgBase + ".svg", inXML}
	p.logf("rendering: %s %s", bin, strings.Join(args, " "))
	if err := p.exec(ctx, bin, args...); err != nil {
		return fmt.Errorf("engraving with verovio failed: %w", err)
	}

	pages, err := filepath.Glob(svgBase + "*.svg")
	if err != nil || len(pages) == 0 {
		return fmt.Errorf("verovio produced no SVG pages in %s", p.workDir)
	}
	sort.Strings(pages)

	conv, err := exec.LookPath("rsvg-convert")
	if err != nil {
		return fmt.Errorf("verovio engraves to SVG and needs rsvg-convert to produce a PDF, " +
			"but it is not on your $PATH -- install it with:\n" +
			"    macOS:  brew install librsvg\n" +
			"    Linux:  sudo apt install librsvg2-bin\n" +
			"    Alternatively install lilypond, which renders PDF directly")
	}

	convArgs := append([]string{"-f", "pdf", "-o", p.Output}, pages...)
	p.logf("converting %d SVG page(s) to PDF", len(pages))
	if err := p.exec(ctx, conv, convArgs...); err != nil {
		return fmt.Errorf("converting engraved SVG to PDF failed: %w", err)
	}
	return nil
}

// convertToLily runs musicxml2ly, which ships alongside LilyPond.
func (p *Pipeline) convertToLily(ctx context.Context, inXML string) (string, error) {
	conv, err := exec.LookPath("musicxml2ly")
	if err != nil {
		return "", fmt.Errorf("lilypond was selected as the renderer but musicxml2ly is not on your $PATH; it ships with LilyPond -- reinstall it, or install verovio instead: %w", err)
	}

	lyFile := filepath.Join(p.workDir, "viola.ly")
	if err := p.exec(ctx, conv, "-o", lyFile, inXML); err != nil {
		return "", fmt.Errorf("converting MusicXML to LilyPond source failed: %w", err)
	}
	return lyFile, nil
}

// exec runs a child process under the pipeline context, capturing its output so
// a failure can quote what the tool actually said rather than a bare exit code.
func (p *Pipeline) exec(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), tessdataEnv()...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Kill the whole process group; OMR tools spawn JVM children that would
	// otherwise survive a cancelled context.
	configureProcessGroup(cmd)

	err := cmd.Run()

	if p.Verbose {
		if s := strings.TrimSpace(stdout.String()); s != "" {
			fmt.Fprintf(os.Stderr, "  [%s stdout] %s\n", filepath.Base(name), s)
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			fmt.Fprintf(os.Stderr, "  [%s stderr] %s\n", filepath.Base(name), s)
		}
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("%s exceeded the %s timeout", filepath.Base(name), p.Timeout)
		}
		return fmt.Errorf("%s was interrupted", filepath.Base(name))
	}

	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w\n%s", filepath.Base(name), err, indent(msg))
		}
		return fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return nil
}

// tessdataEnv points Audiveris at Tesseract language data when the user has not
// set TESSDATA_PREFIX themselves. Without it, OCR silently yields nothing: the
// title, composer and chord symbols are dropped from the score. Audiveris needs
// the legacy-capable models, not the LSTM-only set Homebrew installs.
func tessdataEnv() []string {
	if os.Getenv("TESSDATA_PREFIX") != "" {
		return nil // respect an explicit choice
	}
	for _, dir := range []string{
		os.ExpandEnv("$HOME/.local/share/tessdata"),
		"/opt/homebrew/share/tessdata",
		"/usr/local/share/tessdata",
		"/usr/share/tessdata",
	} {
		if _, err := os.Stat(filepath.Join(dir, "eng.traineddata")); err == nil {
			return []string{"TESSDATA_PREFIX=" + dir}
		}
	}
	return nil
}

// indent offsets captured tool output so it reads as a quoted block.
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func (p *Pipeline) logf(format string, args ...any) {
	if p.Verbose {
		fmt.Fprintf(os.Stderr, "==> "+format+"\n", args...)
	}
}
