package retrievalbench

import (
	"fmt"

	"github.com/tiktoken-go/tokenizer"
)

// TokenCounter counts tokens in tool result text.
type TokenCounter interface {
	Name() string
	Count(text string) (int, error)
}

type tiktokenCounter struct {
	codec tokenizer.Codec
}

// NewO200kCounter counts with the o200k_base encoding, the same tokenizer
// the ecosystem-optimization baseline used, so numbers stay comparable.
func NewO200kCounter() (TokenCounter, error) {
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return nil, fmt.Errorf("load o200k_base tokenizer: %w", err)
	}
	return tiktokenCounter{codec: codec}, nil
}

func (c tiktokenCounter) Name() string { return string(tokenizer.O200kBase) }

func (c tiktokenCounter) Count(text string) (int, error) {
	n, err := c.codec.Count(text)
	if err != nil {
		return 0, fmt.Errorf("count tokens: %w", err)
	}
	return n, nil
}
