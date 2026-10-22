package noaadapter

import (
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestImageProjectRoundTrip(t *testing.T) {
	image := llm.ContentBlock{Type: llm.BlockImage, Source: &llm.ImageSource{Type: "base64", MediaType: "image/png", Data: "private-pixel-data"}}
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("look"), image}}}
	cores, sc := Project(msgs)
	if len(cores) != 2 || strings.Contains(cores[1].Text, image.Source.Data) {
		t.Fatal("unsafe/lost projection", cores)
	}
	out := Reassemble(cores, msgs, sc, ReassembleOptions{})
	if len(out) != 1 || len(out[0].Content) != 2 || out[0].Content[1].Source.Data != image.Source.Data {
		t.Fatal("lost image", out)
	}
	out = Reassemble(cores[:1], msgs, sc, ReassembleOptions{})
	if len(out) != 1 || len(out[0].Content) != 1 {
		t.Fatal("pruned image reintroduced")
	}
}
