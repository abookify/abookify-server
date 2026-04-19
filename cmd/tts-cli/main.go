package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/tts"
)

func main() {
	textFile := flag.String("text", "", "Path to text file (plain text or EPUB)")
	kokoroURL := flag.String("kokoro", "http://localhost:8880", "Kokoro TTS service URL")
	voice := flag.String("voice", "af_heart", "Voice name (e.g. af_heart, am_adam, bf_emma)")
	output := flag.String("output", "", "Output MP3 file path (default: <text>.<voice>.mp3 next to the text file)")
	listVoices := flag.Bool("voices", false, "List available voices and exit")
	flag.Parse()

	client := tts.NewClient(*kokoroURL)

	if *listVoices {
		fmt.Println("Female - American: af_heart, af_bella, af_nicole, af_sarah, af_nova, af_sky, af_river, af_jessica")
		fmt.Println("Male - American:   am_adam, am_michael, am_eric, am_liam, am_puck")
		fmt.Println("Female - British:  bf_emma, bf_lily, bf_alice")
		fmt.Println("Male - British:    bm_george, bm_daniel, bm_lewis")
		return
	}

	if *textFile == "" {
		// Inline text from positional args (no sidecar).
		if flag.NArg() > 0 {
			text := strings.Join(flag.Args(), " ")
			out := *output
			if out == "" {
				out = fmt.Sprintf("tts-%s-%d.mp3", *voice, time.Now().Unix())
			}
			synthesize(client, text, *voice, out, "")
			return
		}
		fmt.Fprintf(os.Stderr, "Usage: tts-cli --text file.txt [--voice af_heart] [--output out.mp3]\n")
		fmt.Fprintf(os.Stderr, "       tts-cli --voices  (list available voices)\n")
		fmt.Fprintf(os.Stderr, "       tts-cli \"Some text to speak\" --output out.mp3\n")
		fmt.Fprintf(os.Stderr, "  If --output is omitted, writes <text-filename>.<voice>.mp3 next to the text file\n")
		fmt.Fprintf(os.Stderr, "  and a .tts.json sidecar with synthesis params alongside.\n")
		os.Exit(1)
	}

	data, err := os.ReadFile(*textFile)
	if err != nil {
		log.Fatalf("Read %s: %v", *textFile, err)
	}

	text := string(data)
	text = library.PreprocessForTTS("", text)

	// Default output: <text-without-ext>.<voice>.mp3 next to the source.
	outPath := *output
	if outPath == "" {
		base := strings.TrimSuffix(*textFile, filepath.Ext(*textFile))
		outPath = fmt.Sprintf("%s.%s.mp3", base, *voice)
	}

	synthesize(client, text, *voice, outPath, *textFile)
}

// synthesize produces the audio and writes a sidecar .tts.json with params.
// sourceFile is included in the sidecar for traceability; empty for inline text.
func synthesize(client *tts.Client, text, voice, output, sourceFile string) {
	if err := client.Health(); err != nil {
		log.Fatalf("Kokoro not reachable: %v", err)
	}

	words := len(strings.Fields(text))
	log.Printf("Synthesizing %d words with voice %q → %s", words, voice, output)

	chunks := splitText(text, 500)
	log.Printf("Split into %d chunks", len(chunks))

	start := time.Now()
	var allAudio []byte
	for i, chunk := range chunks {
		chunkStart := time.Now()
		log.Printf("  chunk %d/%d (%d words)...", i+1, len(chunks), len(strings.Fields(chunk)))
		audio, err := client.Synthesize(chunk, voice)
		if err != nil {
			log.Fatalf("TTS chunk %d failed after %s: %v", i, time.Since(chunkStart).Truncate(time.Second), err)
		}
		allAudio = append(allAudio, audio...)
		// Per-chunk ETA — useful on 100+ chunk books where the run is measured
		// in hours. Reports elapsed + projected remaining based on running avg.
		avg := time.Since(start) / time.Duration(i+1)
		remaining := avg * time.Duration(len(chunks)-i-1)
		log.Printf("  chunk %d done in %s (avg %s/chunk, ~%s remaining)",
			i+1, time.Since(chunkStart).Truncate(time.Second),
			avg.Truncate(time.Second), remaining.Truncate(time.Second))
	}

	if err := os.WriteFile(output, allAudio, 0644); err != nil {
		log.Fatalf("Write %s: %v", output, err)
	}
	elapsed := time.Since(start).Truncate(time.Second)
	log.Printf("Wrote %s (%d bytes, %d words, %s)", output, len(allAudio), words, elapsed)

	// Sidecar JSON describing the synthesis for test fixtures.
	sidecarPath := strings.TrimSuffix(output, filepath.Ext(output)) + ".tts.json"
	sidecar := ttsSidecar{
		OutputMP3:    filepath.Base(output),
		SourceText:   sourceFile,
		Voice:        voice,
		WordCount:    words,
		ChunkCount:   len(chunks),
		OutputBytes:  len(allAudio),
		ElapsedSecs:  elapsed.Seconds(),
		KokoroURL:    client.BaseURL(),
		SynthesizedAt: time.Now().Format(time.RFC3339),
	}
	sidecarData, _ := json.MarshalIndent(sidecar, "", "  ")
	if err := os.WriteFile(sidecarPath, sidecarData, 0644); err != nil {
		log.Printf("Warning: couldn't write sidecar %s: %v", sidecarPath, err)
	} else {
		log.Printf("Wrote %s", sidecarPath)
	}
}

// ttsSidecar is the JSON emitted next to each TTS output for test fixtures.
type ttsSidecar struct {
	OutputMP3     string  `json:"output_mp3"`
	SourceText    string  `json:"source_text,omitempty"`
	Voice         string  `json:"voice"`
	WordCount     int     `json:"word_count"`
	ChunkCount    int     `json:"chunk_count"`
	OutputBytes   int     `json:"output_bytes"`
	ElapsedSecs   float64 `json:"elapsed_secs"`
	KokoroURL     string  `json:"kokoro_url"`
	SynthesizedAt string  `json:"synthesized_at"`
}

func splitText(text string, targetWords int) []string {
	words := strings.Fields(text)
	if len(words) <= targetWords {
		return []string{text}
	}

	var chunks []string
	sentences := strings.Split(text, ".")
	var current strings.Builder
	currentWords := 0

	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		sWords := len(strings.Fields(s))
		if currentWords+sWords > targetWords && currentWords > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			currentWords = 0
		}
		if current.Len() > 0 {
			current.WriteString(". ")
		}
		current.WriteString(s)
		currentWords += sWords
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}
