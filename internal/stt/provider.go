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

// The existing Client already satisfies Provider — just add Name().
func (c *Client) Name() string { return "whisper-local" }
