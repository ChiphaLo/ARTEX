package llm

import "encoding/json"

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
		raw, err := json.Marshal(msg)
		if err != nil {
			return 0, err
		}
		total += len(raw) + MessageFramingBytes
	}
	return total, nil
}
