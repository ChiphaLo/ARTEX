package llm

import (
	"encoding/json"
	"strings"
)

// RequestBytesPerToken is a conservative UTF-8 estimate, not a model tokenizer.
const RequestBytesPerToken = 3

const MessageFramingBytes = 13

// EstimateRequestBytes includes messages, fixed instructions, schemas and framing.
// Budget gates and summary batching must use the same estimate.
func EstimateRequestBytes(req CompletionRequest) (int, error) {
	msgs := req.Messages
	req.Messages = nil
	raw, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	total := len(raw) + 64
	for _, msg := range msgs {
		msg.Content = budgetImageBlocks(msg.Content)
		raw, err := json.Marshal(msg)
		if err != nil {
			return 0, err
		}
		total += len(raw) + MessageFramingBytes
	}
	return total, nil
}

// Avoid treating encoded pixels as millions of text tokens. This is an estimate;
// actual vision usage is reported by the provider.
func budgetImageBlocks(in []ContentBlock) []ContentBlock {
	out := append([]ContentBlock(nil), in...)
	for i := range out {
		if out[i].Type == BlockImage {
			out[i] = TextBlock(strings.Repeat(" ", 8192*RequestBytesPerToken))
		}
		if len(out[i].Content) > 0 {
			out[i].Content = budgetImageBlocks(out[i].Content)
		}
	}
	return out
}
