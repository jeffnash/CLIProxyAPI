package executor

import (
	"bytes"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestComposerReseedCarriesToolResultImage pins EX3 (reseed half): a Read-tool image lives in
// inp["results"][].images (composerToolResultEntry), NOT inp["images"] (which only carries trailing role:user
// images). The lost-continuation reseed drives a fresh type:"user" turn whose images come solely from
// seed["images"], so it must gather the per-result images too — else the image is silently dropped on reseed
// and the model re-reads the same file ("can't read photos from a file").
func TestComposerReseedCarriesToolResultImage(t *testing.T) {
	// openai-format continuation (what the claude->openai translator produces): user opener, assistant Read
	// tool_call, then a role:tool result whose content is the image as a data: URI image_url part.
	oai := []byte(`{"messages":[
		{"role":"user","content":"read /tmp/x.png and tell me the exact text"},
		{"role":"assistant","content":"","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x.png\"}"}}]},
		{"role":"tool","tool_call_id":"toolu_1","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,IMGDATA123"}}]}
	]}`)
	_, body, ok := composerReseedLostContinuation("tenant", "key", oai, cliproxyexecutor.Options{}, composerContinuationHint{}, "composer-2.5", nil, "", nil)
	if !ok {
		t.Fatal("expected a reseed for a tool-result continuation carrying an opener")
	}
	if !bytes.Contains(body, []byte("IMGDATA123")) {
		t.Fatalf("reseed body dropped the tool-result image data; body=%s", body)
	}
	if !bytes.Contains(body, []byte("image/png")) {
		t.Fatalf("reseed body dropped the tool-result image mimeType; body=%s", body)
	}
	// The reseed must also tell the model the image is attached (else it re-calls the tool, since the replayed
	// history still says "read the file" with the tool advertised).
	if !bytes.Contains(body, []byte("attached directly to this message")) {
		t.Fatalf("reseed body missing the image directive (model would re-call the tool); body=%s", body)
	}
}
