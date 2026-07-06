package common

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestMergeAdjacentMessagesSkipsNonStringTextBlocks(t *testing.T) {
	messages := []gjson.Result{
		gjson.Parse(`{"role":"user","content":[{"type":"text","text":123}]}`),
		gjson.Parse(`{"role":"user","content":[{"type":"text","text":"ok"}]}`),
	}

	merged := MergeAdjacentMessages(messages)
	if len(merged) != 1 {
		t.Fatalf("merged len = %d, want 1", len(merged))
	}
	content := merged[0].Get("content")
	if got := content.Get("0.text").Int(); got != 123 {
		t.Fatalf("first text value = %d, want 123", got)
	}
	if got := content.Get("1.text").String(); got != "ok" {
		t.Fatalf("second text value = %q, want ok", got)
	}
}
