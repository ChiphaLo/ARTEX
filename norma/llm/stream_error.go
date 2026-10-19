package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Chat Completions gateways may report an error inside an HTTP-200 SSE stream.
// Do not silently drop it and later replace its code/details with "empty reply".
func openAIStreamError(event, data string) error {
	if event == "error" {
		return fmt.Errorf("openai: upstream error event:\n%s", data)
	}
	if !strings.Contains(data, `"error"`) {
		return nil
	}
	var frame struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(data), &frame) == nil && len(frame.Error) > 0 && string(frame.Error) != "null" {
		return fmt.Errorf("openai: upstream error event:\n%s", data)
	}
	return nil
}
