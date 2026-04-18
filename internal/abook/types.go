package abook

// Manifest is the top-level manifest.json in an .abook file.
type Manifest struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Language  string    `json:"language"`
	Created   string    `json:"created"`
	Generator string    `json:"generator"`
	Chapters  []Chapter `json:"chapters"`
	TTSVoice  string    `json:"tts_voice,omitempty"`
	STTModel  string    `json:"stt_model,omitempty"`
	Source    *Source    `json:"source,omitempty"`
}

type Chapter struct {
	Index       int     `json:"index"`
	Title       string  `json:"title"`
	Text        string  `json:"text,omitempty"`
	Audio       string  `json:"audio,omitempty"`
	Sync        string  `json:"sync,omitempty"`
	DurationSec float64 `json:"duration_secs,omitempty"`
	WordCount   int     `json:"word_count,omitempty"`
}

type Source struct {
	TextOrigin  string `json:"text_origin,omitempty"`
	AudioOrigin string `json:"audio_origin,omitempty"`
}

// SyncData holds word-level timestamps for a chapter.
type SyncData struct {
	Format  string      `json:"format"`
	Version int         `json:"version"`
	Words   []SyncWord  `json:"words"`
}

// SyncWord is [start, end, "word"] — but we use a struct for Go, serialize as array.
type SyncWord struct {
	Start float64
	End   float64
	Word  string
}
