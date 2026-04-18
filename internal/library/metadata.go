package library

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dhowden/tag"
)

// Metadata holds extracted information from a media file.
type Metadata struct {
	Title    string
	Author   string
	Album    string
	Duration float64 // seconds; 0 if unavailable
}

// probeDuration shells out to ffprobe to get a file's duration in seconds.
// Returns 0 on any failure — the caller treats it as "unknown" rather than error.
func probeDuration(path string) float64 {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path).Output()
	if err != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return d
}

// ExtractMP3Metadata reads ID3 tags from an audio file.
func ExtractMP3Metadata(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return Metadata{}, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	m, err := tag.ReadFrom(f)
	if err != nil {
		return Metadata{}, fmt.Errorf("read tags: %w", err)
	}

	return Metadata{
		Title:    m.Title(),
		Author:   m.Artist(),
		Album:    m.Album(),
		Duration: probeDuration(path),
	}, nil
}

// epubContainer represents the META-INF/container.xml structure.
type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

// epubPackage represents the OPF package document.
type epubPackage struct {
	Metadata struct {
		Title   []string `xml:"title"`
		Creator []string `xml:"creator"`
	} `xml:"metadata"`
}

// ExtractEPUBMetadata reads metadata from an EPUB file.
func ExtractEPUBMetadata(path string) (Metadata, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return Metadata{}, fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	// Find the rootfile path from container.xml
	opfPath, err := findOPFPath(&r.Reader)
	if err != nil {
		return Metadata{}, err
	}

	// Read and parse the OPF file
	opfFile, err := findInZip(&r.Reader, opfPath)
	if err != nil {
		return Metadata{}, fmt.Errorf("find OPF: %w", err)
	}

	rc, err := opfFile.Open()
	if err != nil {
		return Metadata{}, fmt.Errorf("open OPF: %w", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return Metadata{}, fmt.Errorf("read OPF: %w", err)
	}

	var pkg epubPackage
	if err := xml.Unmarshal(data, &pkg); err != nil {
		return Metadata{}, fmt.Errorf("parse OPF: %w", err)
	}

	var meta Metadata
	if len(pkg.Metadata.Title) > 0 {
		meta.Title = pkg.Metadata.Title[0]
	}
	if len(pkg.Metadata.Creator) > 0 {
		meta.Author = pkg.Metadata.Creator[0]
	}

	return meta, nil
}

func findOPFPath(r *zip.Reader) (string, error) {
	containerFile, err := findInZip(r, "META-INF/container.xml")
	if err != nil {
		return "", fmt.Errorf("find container.xml: %w", err)
	}

	rc, err := containerFile.Open()
	if err != nil {
		return "", fmt.Errorf("open container.xml: %w", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}

	var c epubContainer
	if err := xml.Unmarshal(data, &c); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}

	if len(c.Rootfiles) == 0 {
		return "", fmt.Errorf("no rootfiles in container.xml")
	}

	return c.Rootfiles[0].FullPath, nil
}

func findInZip(r *zip.Reader, name string) (*zip.File, error) {
	// Try exact match first, then case-insensitive
	for _, f := range r.File {
		if f.Name == name {
			return f, nil
		}
	}
	for _, f := range r.File {
		if strings.EqualFold(f.Name, name) {
			return f, nil
		}
	}
	return nil, fmt.Errorf("file %q not found in archive", name)
}

// ExtractMetadata detects the format by extension and extracts metadata.
func ExtractMetadata(path string) (Metadata, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3", ".m4b", ".m4a", ".flac", ".aac":
		return ExtractMP3Metadata(path)
	case ".epub":
		return ExtractEPUBMetadata(path)
	default:
		return Metadata{}, nil
	}
}
