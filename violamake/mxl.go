package main

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MusicXML travels in two forms: plain .xml/.musicxml, and .mxl -- a ZIP
// container holding the score plus a META-INF/container.xml pointing at it.
// Audiveris exports the compressed form by default, so the pipeline has to
// unwrap it before the streaming transposer can read a single XML document.

// container mirrors META-INF/container.xml, which names the real score entry.
type container struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

// isCompressed reports whether a path looks like a zipped MusicXML container.
func isCompressed(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".mxl")
}

// extractMXL unwraps a .mxl archive into a plain MusicXML file inside dir and
// returns the extracted path. It honors META-INF/container.xml rather than
// guessing, falling back to the first plausible entry when the container is
// absent or unreadable.
func extractMXL(src, dir string) (string, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return "", fmt.Errorf("opening compressed MusicXML %s: %w", filepath.Base(src), err)
	}
	defer zr.Close()

	entry := findRootfile(&zr.Reader)
	if entry == nil {
		return "", fmt.Errorf("no MusicXML document found inside %s", filepath.Base(src))
	}

	rc, err := entry.Open()
	if err != nil {
		return "", fmt.Errorf("reading %s from archive: %w", entry.Name, err)
	}
	defer rc.Close()

	out := filepath.Join(dir, "uncompressed.musicxml")
	f, err := os.Create(out)
	if err != nil {
		return "", fmt.Errorf("creating uncompressed score: %w", err)
	}

	// Cap the copy so a malformed or hostile archive cannot exhaust the disk.
	const maxScore = 512 << 20
	if _, err := io.Copy(f, io.LimitReader(rc, maxScore)); err != nil {
		f.Close()
		return "", fmt.Errorf("extracting %s: %w", entry.Name, err)
	}
	// Report a failed close: it can mean the write never reached disk.
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing uncompressed score: %w", err)
	}
	return out, nil
}

// findRootfile locates the score entry named by META-INF/container.xml, or the
// first top-level XML entry if the container is missing.
func findRootfile(zr *zip.Reader) *zip.File {
	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		byName[f.Name] = f
	}

	if cf, ok := byName["META-INF/container.xml"]; ok {
		if rc, err := cf.Open(); err == nil {
			defer rc.Close()
			var c container
			if err := xml.NewDecoder(rc).Decode(&c); err == nil {
				for _, rf := range c.Rootfiles {
					// The name is only ever used to look up an entry already in
					// the archive, never as a path on disk, but reject obvious
					// traversal attempts so nothing downstream can misread it.
					if strings.Contains(rf.FullPath, "..") || filepath.IsAbs(rf.FullPath) {
						continue
					}
					if f, ok := byName[rf.FullPath]; ok {
						return f
					}
				}
			}
		}
	}

	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "META-INF/") {
			continue
		}
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".xml", ".musicxml":
			return f
		}
	}
	return nil
}
