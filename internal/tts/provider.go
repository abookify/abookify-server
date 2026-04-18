// TTS provider interface. Abstracts over local (Kokoro) and cloud (OpenAI)
// TTS engines so the generator can use whichever the user configures.
package tts

// Provider is the interface any TTS engine must implement.
type Provider interface {
	Name() string
	Health() error
	Synthesize(text string, voice string) ([]byte, error)
}

// The existing Client already satisfies Provider — just add Name().
func (c *Client) Name() string { return "kokoro" }
