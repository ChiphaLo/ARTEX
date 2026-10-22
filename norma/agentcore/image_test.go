package agentcore

import (
	"context"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestImagePromptPersistAndResume(t *testing.T) {
	srv := textServer(t, "answer")
	defer srv.Close()
	provider, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	store := transcript.NewStore(t.TempDir())
	opts := Options{Provider: provider, Transcript: store, Tools: []tool.CoreTool{}, WorkingDir: t.TempDir()}
	s := NewSession(opts)
	defer s.Close()
	image := llm.ContentBlock{Type: llm.BlockImage, Source: &llm.ImageSource{Type: "base64", MediaType: "image/png", Data: "aW1hZ2U="}}
	for _, err := range s.PromptWithContent(context.Background(), "describe image", []llm.ContentBlock{image}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	resumed := NewSession(opts)
	defer resumed.Close()
	if err := resumed.Resume(s.SessionID()); err != nil {
		t.Fatal(err)
	}
	msgs := resumed.Messages()
	if len(msgs) < 1 || len(msgs[0].Content) != 2 || msgs[0].Content[1].Source.Data != image.Source.Data {
		t.Fatal("image lost on resume", msgs)
	}
	if err := store.ExcludeSelection(s.SessionID(), "user", "describe image", ""); err != nil {
		t.Fatal(err)
	}
	excluded := NewSession(opts)
	defer excluded.Close()
	if err := excluded.Resume(s.SessionID()); err != nil {
		t.Fatal(err)
	}
	for _, m := range excluded.Messages() {
		for _, b := range m.Content {
			if b.Type == llm.BlockImage {
				t.Fatal("removed user image still in context")
			}
		}
	}

}
