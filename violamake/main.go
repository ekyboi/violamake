// Command violamake converts violin sheet music into a viola part.
//
// It reads a PDF, recognizes the score, rewrites treble clef as alto clef and
// transposes every pitch down a perfect fifth, then engraves the result as a
// new PDF beside the original. The fifth is chosen so the violin's E-A-D-G
// strings map onto the viola's A-D-G-C: the written notes, and therefore the
// fingerings, stay exactly where a violinist's hands expect them.
//
//	violamake sonata.pdf              -> sonata-viola.pdf
//	violamake --preserve-pitch x.pdf  -> clef swap only, pitches untouched
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	outputSuffix   = "-viola"
	defaultTimeout = 10 * time.Minute
)

func main() {
	// All real work happens in run() so that deferred cleanup executes before
	// the process exits -- os.Exit skips defers.
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "violamake: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	preservePitch := flag.Bool("preserve-pitch", false,
		"swap the clef to alto but leave pitches untouched (skip the fifth transposition)")
	timeout := flag.Duration("timeout", defaultTimeout,
		"maximum duration for the whole conversion")
	verbose := flag.Bool("v", false, "print each pipeline stage and any tool output")
	outPath := flag.String("o", "", "explicit output path (default: <input>-viola.pdf beside the input)")

	flag.Usage = usage
	flag.Parse()

	// The input is positional, not a flag: the tool is meant to be run from
	// inside the folder holding the music.
	if flag.NArg() == 0 {
		usage()
		return errors.New("no input file given")
	}
	if flag.NArg() > 1 {
		return fmt.Errorf("expected exactly one input file, got %d: %s\n"+
			"       if your filename contains spaces, quote it: violamake \"my score.pdf\"",
			flag.NArg(), strings.Join(flag.Args(), " "))
	}

	input, err := resolveInput(flag.Arg(0))
	if err != nil {
		return err
	}

	output := *outPath
	if output == "" {
		output = deriveOutput(input)
	} else {
		// resolveInput returns an absolute path, so -o must be made absolute
		// too before the two can be compared meaningfully.
		abs, err := filepath.Abs(output)
		if err != nil {
			return fmt.Errorf("resolving output path %q: %w", output, err)
		}
		output = abs
	}
	if sameFile(input, output) {
		return fmt.Errorf("refusing to overwrite the input file %s", flag.Arg(0))
	}

	pipeline, err := NewPipeline(input, output, *preservePitch, *timeout, *verbose)
	if err != nil {
		return err
	}
	// Runs on every ordinary return path, including errors.
	defer pipeline.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Watch for interrupts on a channel. The first signal cancels the context,
	// which kills child processes and lets Run return so cleanup happens
	// normally. A second signal aborts immediately, after one last sweep of the
	// temp directory, for a user who is out of patience.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-ctx.Done():
			return
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "\nviolamake: received %s, cleaning up...\n", sig)
			cancel()
			select {
			case <-sigCh:
				fmt.Fprintln(os.Stderr, "violamake: second interrupt, exiting now")
				pipeline.Cleanup()
				os.Exit(130) // 128 + SIGINT
			case <-ctx.Done():
			}
		}
	}()

	// Note whether the destination already holds a file. On failure only a file
	// this run created may be removed: deleting something that was already
	// there would destroy the user's data over an error we caused.
	_, preexisting := os.Stat(output)
	existedBefore := preexisting == nil

	fmt.Fprintf(os.Stderr, "violamake: %s -> %s\n", filepath.Base(input), filepath.Base(output))

	stats, err := pipeline.Run(ctx)
	if err != nil {
		// A half-written PDF would look like a success, so clear it -- but only
		// if the run created it.
		if !existedBefore {
			if rmErr := os.Remove(output); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "warning: could not remove incomplete output %s: %v\n", output, rmErr)
			}
		} else {
			fmt.Fprintf(os.Stderr, "note: %s was left as it was found\n", output)
		}
		return err
	}

	mode := "transposed down a perfect fifth"
	if *preservePitch {
		mode = "clef swapped, pitch preserved"
	}
	fmt.Fprintf(os.Stderr, "violamake: wrote %s (%s)\n", output, mode)
	if *verbose {
		fmt.Fprintf(os.Stderr, "violamake: %s\n", stats)
	}
	return nil
}

// sameFile reports whether two paths lead to the same file. Comparing the
// cleaned strings is not enough: a symlink, or a case-insensitive filesystem
// such as the macOS default, can reach one file by two different names, and
// writing the output over the input would destroy the user's score.
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false // b does not exist yet, so it cannot be a
	}
	return os.SameFile(ai, bi)
}

// resolveInput validates the positional argument and returns an absolute path.
func resolveInput(arg string) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", arg, err)
	}

	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no such file: %s", arg)
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", arg, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a score file", arg)
	}

	// MusicXML input is accepted too, which lets the transposer be used on its
	// own without paying for an OMR pass.
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".pdf", ".xml", ".musicxml", ".mxl":
	default:
		return "", fmt.Errorf("unsupported input type %q -- expected a .pdf, .musicxml, .xml or .mxl file",
			filepath.Ext(abs))
	}
	return abs, nil
}

// deriveOutput turns /music/sonata.pdf into /music/sonata-viola.pdf, keeping the
// result in the same directory as the input.
func deriveOutput(input string) string {
	dir := filepath.Dir(input)
	base := filepath.Base(input)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	// Running the tool twice should not yield sonata-viola-viola.pdf.
	stem = strings.TrimSuffix(stem, outputSuffix)

	return filepath.Join(dir, stem+outputSuffix+".pdf")
}

func usage() {
	fmt.Fprint(os.Stderr, `violamake -- turn violin sheet music into a viola part

Usage:
  violamake [flags] <score.pdf>

Reads the score, rewrites treble clef as alto clef, transposes every pitch down
a perfect fifth, and writes <score>-viola.pdf in the same directory. The fifth
maps the violin's E-A-D-G strings onto the viola's A-D-G-C, so fingerings carry
over unchanged.

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
Examples:
  violamake sonata.pdf                  write sonata-viola.pdf
  violamake --preserve-pitch duet.pdf   clef swap only, no transposition
  violamake -o part2.pdf score.pdf      choose the output path explicitly

Requires Audiveris for recognition (PDF input only) and Verovio or LilyPond to
engrave the result. Run violamake with a PDF to see exact install instructions
for anything missing.
`)
}
