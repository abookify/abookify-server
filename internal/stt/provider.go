// STT provider interface. Abstracts over local (faster-whisper) and cloud
// (OpenAI Whisper API) speech-to-text engines.
package stt

// Provider is the interface any STT engine must implement.
type Provider interface {
	Name() string
	Health() error
	// Info reports the active model + compute device (the device readout the
	// server surfaces). Cloud providers return a "cloud" device.
	Info() (*Info, error)
	TranscribeFile(audioPath string) (*TranscribeResult, error)
}

// Unloader is implemented by local engines that can free their model from
// memory when idle and reload it on the next request. Cloud providers have no
// local model to free, so they don't implement it — the idle monitor simply
// skips any provider that isn't an Unloader.
type Unloader interface {
	// Unload frees the model from RAM/VRAM. Returns whether a model was actually
	// unloaded (false if it was already unloaded).
	Unload() (bool, error)
}

// The existing Client already satisfies Provider — just add Name().
func (c *Client) Name() string { return "whisper-local" }
