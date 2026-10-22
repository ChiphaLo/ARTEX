package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImageWireFormats(t *testing.T) {
	image := ContentBlock{Type: BlockImage, Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "aW1hZ2U="}}
	req := CompletionRequest{Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("look"), image, TextBlock("then explain")}}}}
	for _, stream := range []bool{false, true} {
		for _, format := range []string{"responses", "chat", "anthropic"} {
			var raw []byte
			var err error
			switch format {
			case "responses":
				raw, err = (&openaiResponsesProvider{}).buildBody(req, stream)
			case "chat":
				raw, err = (&openaiProvider{}).buildBody(req, stream)
			case "anthropic":
				raw, err = (&anthropicProvider{}).buildBody(req, stream)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(raw) || !strings.Contains(string(raw), "aW1hZ2U=") || !strings.Contains(string(raw), "look") {
				t.Fatalf("%s lost image/text: %s", format, raw)
			}
			expected := map[string]string{"responses": "input_image", "chat": "image_url", "anthropic": "media_type"}[format]
			if !strings.Contains(string(raw), expected) {
				t.Fatalf("%s wrong image shape: %s", format, raw)
			}
		}
	}
	raw, _ := json.Marshal(req.Messages[0])
	var round Message
	if err := json.Unmarshal(raw, &round); err != nil || round.Content[1].Source.Data != image.Source.Data {
		t.Fatalf("transcript image lost: %v", err)
	}
}
func TestImageBudgetDoesNotCountBase64AsText(t *testing.T) {
	req := CompletionRequest{Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: BlockImage, Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: strings.Repeat("A", 4<<20)}}}}}}
	n, err := EstimateRequestBytes(req)
	if err != nil || n > 30000 || n < 8192*3 {
		t.Fatalf("budget=%d err=%v", n, err)
	}
	if len(req.Messages[0].Content[0].Source.Data) != 4<<20 {
		t.Fatal("estimation mutated caller image")
	}
}
