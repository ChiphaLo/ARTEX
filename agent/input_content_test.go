package agent

import (
	"context"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestInputContentOnceAndCachedContextTransfer(t *testing.T) {
	n := 0
	source := WithInputContent(context.Background(), func() ([]llm.ContentBlock, error) { n++; return []llm.ContentBlock{{Type: llm.BlockImage}}, nil })
	ctx := InheritInputContent(context.Background(), source)
	b, e := takeInputContent(ctx)
	if e != nil || len(b) != 1 || n != 1 {
		t.Fatal(b, e, n)
	}
	b, e = takeInputContent(ctx)
	if e != nil || len(b) != 0 || n != 1 {
		t.Fatal("reused by settlement", b, e, n)
	}
	ctx = InheritInputContent(source, context.Background())
	b, e = takeInputContent(ctx)
	if e != nil || len(b) != 0 {
		t.Fatal("inherited stale images")
	}
}
