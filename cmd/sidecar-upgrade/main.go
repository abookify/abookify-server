// sidecar-upgrade: bring .stt.json files up to the latest schema version.
//
// Usage:
//   sidecar-upgrade <path>...        # upgrade one or more files in place
//   sidecar-upgrade -dir <directory> # upgrade every *.stt.json under a tree
//   sidecar-upgrade -check <path>    # report version without rewriting
//
// The actual upgrade logic lives in internal/library — this CLI is a thin
// wrapper so batch jobs and CI can invoke it directly. The server itself
// auto-upgrades on read via library.ReadSidecar, so this tool is for
// proactive bulk migration (or testing).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/library"
)

func main() {
	checkOnly := flag.Bool("check", false, "report current version without rewriting")
	dir := flag.String("dir", "", "recursively upgrade every *.stt.json under this directory")
	flag.Parse()

	var paths []string
	if *dir != "" {
		err := filepath.WalkDir(*dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(strings.ToLower(p), ".stt.json") {
				paths = append(paths, p)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "walk %s: %v\n", *dir, err)
			os.Exit(1)
		}
	}
	paths = append(paths, flag.Args()...)

	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "usage: sidecar-upgrade [-check] [-dir <directory>] <path>...\n")
		os.Exit(2)
	}

	upgraded, alreadyV3, failed := 0, 0, 0
	for _, p := range paths {
		ver, err := peekVersion(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: read: %v\n", p, err)
			failed++
			continue
		}
		if *checkOnly {
			fmt.Printf("%s\tv%d\n", p, ver)
			continue
		}
		if ver == library.SchemaVersion {
			alreadyV3++
			continue
		}
		// ReadSidecar runs the upgrade chain and rewrites the file.
		if _, err := library.ReadSidecar(p); err != nil {
			fmt.Fprintf(os.Stderr, "%s: upgrade: %v\n", p, err)
			failed++
			continue
		}
		fmt.Printf("upgraded %s: v%d → v%d\n", p, ver, library.SchemaVersion)
		upgraded++
	}

	if !*checkOnly {
		fmt.Printf("\nsummary: %d upgraded, %d already v%d, %d failed\n",
			upgraded, alreadyV3, library.SchemaVersion, failed)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func peekVersion(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	if probe.Version == 0 {
		// v1 had no version field at all.
		return 1, nil
	}
	return probe.Version, nil
}
