// Command abook is the standalone companion CLI for the .abook format —
// inspect, extract, and pack .abook containers without a running server.
//
//	abook info <file.abook> [--json]   print manifest + source/file summary
//	abook extract <file.abook> [dir]   unzip the archive (default: <name>/)
//	abook pack <dir> [out.abook]        build a .abook from an unpacked dir
//
// A .abook is a standard deflate ZIP (manifest.json + a per-work book.db +
// optional cover + optional bundled audio), so `abook extract` output is also
// readable by any stock unzip tool.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/abook"
)

var version = "dev" // set via -ldflags -X main.version

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "info":
		err = cmdInfo(args)
	case "extract":
		err = cmdExtract(args)
	case "pack":
		err = cmdPack(args)
	case "version", "--version", "-v":
		fmt.Printf("abook %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "abook: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "abook: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `abook — companion CLI for the .abook format

Usage:
  abook info <file.abook> [--json]   print manifest + source/file summary
  abook extract <file.abook> [dir]   unzip the archive (default: <name>/)
  abook pack <dir> [out.abook]       build a .abook from an unpacked directory
  abook version

A .abook is a standard deflate ZIP; extracted output opens in any unzip tool.
`)
}

func cmdInfo(args []string) error {
	asJSON := false
	var path string
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else if path == "" {
			path = a
		}
	}
	if path == "" {
		return fmt.Errorf("usage: abook info <file.abook> [--json]")
	}
	info, err := abook.Inspect(path)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	printInfo(path, info)
	return nil
}

func printInfo(path string, info *abook.ArchiveInfo) {
	m := info.Manifest
	fmt.Printf("%s\n", filepath.Base(path))
	if m == nil {
		fmt.Println("  (no manifest.json — not a v2 .abook?)")
	} else {
		fmt.Printf("  Title       : %s\n", m.Title)
		fmt.Printf("  Author      : %s\n", m.Author)
		fmt.Printf("  Format      : abook v%d  (source: %s)\n", m.Version, m.SourceKind)
		fmt.Printf("  Schema/Ver  : book.db v%d  ·  content %s\n", m.SchemaVersion, orDash(m.ContentVersion))
		if m.CoveragePct != nil {
			fmt.Printf("  Alignment   : %.1f%% coverage  (%s/%s)\n", *m.CoveragePct, orDash(deref(m.AlignMethod)), orDash(deref(m.AlignUnit)))
		}
		fmt.Printf("  Embeddings  : %s\n", embStr(m))
		if m.HasAudio {
			fmt.Printf("  Audio       : bundled\n")
		} else {
			fmt.Printf("  Audio       : not bundled (streams from your server)\n")
		}
		if m.HasOriginalEbook {
			names := make([]string, 0, len(m.Originals))
			for _, o := range m.Originals {
				names = append(names, o.Filename)
			}
			fmt.Printf("  Original    : %s\n", strings.Join(names, ", "))
		} else {
			fmt.Printf("  Original    : not bundled\n")
		}
		fmt.Printf("  Generator   : %s\n", m.Generator)
	}

	if len(info.Sources) > 0 {
		fmt.Printf("\n  Sources (%d):\n", len(info.Sources))
		for _, s := range info.Sources {
			extra := ""
			if s.MediaType == "text" && s.Chapters > 0 {
				extra = fmt.Sprintf(", %d chapters", s.Chapters)
			}
			if s.Bundled {
				extra += ", audio bundled"
			}
			vis := ""
			if s.Visibility != "" && s.Visibility != "visible" {
				vis = " [" + s.Visibility + "]"
			}
			fmt.Printf("    · %-10s %-18s %s%s%s\n", s.Format, s.Origin, s.Filename, extra, vis)
		}
	}
	if info.Chunks > 0 {
		fmt.Printf("\n  RAG chunks  : %d  (%d with embeddings)  ·  %d paragraphs\n", info.Chunks, info.EmbeddedChunks, info.Paragraphs)
	}

	fmt.Printf("\n  Files (%d):\n", len(info.Files))
	for _, f := range info.Files {
		fmt.Printf("    %-22s %10s  (%s)\n", f.Name, humanBytes(f.Size), f.Method)
	}
	if !info.StandardDeflate {
		fmt.Println("\n  ! warning: contains non-deflate entries (may not open in all tools)")
	}
}

func cmdExtract(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: abook extract <file.abook> [dir]")
	}
	path := args[0]
	outDir := ""
	if len(args) >= 2 {
		outDir = args[1]
	} else {
		base := filepath.Base(path)
		outDir = strings.TrimSuffix(base, filepath.Ext(base))
	}
	written, err := abook.ExtractTo(path, outDir)
	if err != nil {
		return err
	}
	fmt.Printf("Extracted %d files to %s/\n", len(written), outDir)
	return nil
}

func cmdPack(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: abook pack <dir> [out.abook]")
	}
	srcDir := strings.TrimRight(args[0], "/")
	outPath := ""
	if len(args) >= 2 {
		outPath = args[1]
	} else {
		outPath = filepath.Base(srcDir) + ".abook"
	}
	if err := abook.PackDir(srcDir, outPath); err != nil {
		return err
	}
	if fi, err := os.Stat(outPath); err == nil {
		fmt.Printf("Packed %s  (%s)\n", outPath, humanBytes(fi.Size()))
	} else {
		fmt.Printf("Packed %s\n", outPath)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func embStr(m *abook.Manifest) string {
	if !m.HasEmbeddings {
		return "none (keyword-only)"
	}
	if m.EmbeddingModel != "" {
		return fmt.Sprintf("yes — %s (%d-dim)", m.EmbeddingModel, m.EmbeddingDim)
	}
	if m.EmbeddingDim > 0 {
		return fmt.Sprintf("yes (%d-dim)", m.EmbeddingDim)
	}
	return "yes"
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
