package server

// Option changes how the chunk API handler behaves.
type Option func(*handlerSettings)

type handlerSettings struct {
	checkDigests bool // Refuse writes whose bytes do not hash to the chunk name
}

// WithDigestCheck makes the handler refuse a chunk whose bytes do not hash to its name.
func WithDigestCheck() Option {
	return func(settings *handlerSettings) { settings.checkDigests = true }
}
