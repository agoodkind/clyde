package config

import "errors"

const defaultMaxChatRequestBodyBytes = 32 << 20

var errInvalidChatRequestBodyLimit = errors.New(
	"adapter.max_chat_request_body_bytes must be greater than zero",
)

// ChatRequestBodyLimit returns the effective limit for Chat Completions.
func (cfg AdapterConfig) ChatRequestBodyLimit() int64 {
	if cfg.MaxChatRequestBodyBytes == nil {
		return defaultMaxChatRequestBodyBytes
	}

	return *cfg.MaxChatRequestBodyBytes
}

func (cfg AdapterConfig) validateChatRequestBodyLimit() error {
	if cfg.ChatRequestBodyLimit() <= 0 {
		return errInvalidChatRequestBodyLimit
	}

	return nil
}
