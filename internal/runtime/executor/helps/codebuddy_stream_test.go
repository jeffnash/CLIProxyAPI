package helps

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func readCodeBuddyStreamFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/codebuddy/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

// byteDribbleReader returns one byte per Read to split UTF-8 and JSON escape
// sequences across reads.
type byteDribbleReader struct {
	raw []byte
	pos int
}

func (r *byteDribbleReader) Read(out []byte) (int, error) {
	if r.pos >= len(r.raw) {
		return 0, io.EOF
	}
	if len(out) == 0 {
		return 0, nil
	}
	out[0] = r.raw[r.pos]
	r.pos++
	return 1, nil
}

func drainCodeBuddyStream(t *testing.T, s *CodeBuddyStream) [][]byte {
	t.Helper()
	var chunks [][]byte
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			return chunks
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		chunks = append(chunks, chunk)
	}
}

func TestAggregateCodeBuddyStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/codebuddy/text.sse")
	if err != nil {
		t.Fatal(err)
	}
	got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(bytes.NewReader(raw), "hy4-preview"))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"choices.0.message.content": "Hello",
		"choices.0.finish_reason":   "stop",
		"model":                     "hy4-preview",
	} {
		if value := gjson.GetBytes(got, path).String(); value != want {
			t.Fatalf("%s = %q, want %q", path, value, want)
		}
	}
	if got := gjson.GetBytes(got, "usage.total_tokens").Int(); got != 9 {
		t.Fatalf("total_tokens = %d, want 9", got)
	}
}

func TestCodeBuddyStreamFraming(t *testing.T) {
	raw := readCodeBuddyStreamFixture(t, "text.sse")
	for name, reader := range map[string]io.Reader{
		"whole":    bytes.NewReader(raw),
		"one byte": &byteDribbleReader{raw: raw},
		"crlf":     bytes.NewReader(bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))),
	} {
		chunks := drainCodeBuddyStream(t, NewCodeBuddyStream(reader, "hy4-preview"))
		if len(chunks) != 4 {
			t.Fatalf("%s: chunks = %d", name, len(chunks))
		}
		for _, chunk := range chunks {
			if bytes.HasPrefix(bytes.TrimSpace(chunk), []byte("data:")) {
				t.Fatalf("%s: framing leaked: %s", name, chunk)
			}
		}
	}
	t.Run("comments ignored", func(t *testing.T) {
		raw := ": heartbeat\n\n" + string(readCodeBuddyStreamFixture(t, "text.sse"))
		chunks := drainCodeBuddyStream(t, NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if len(chunks) != 4 {
			t.Fatalf("chunks = %d", len(chunks))
		}
	})
	t.Run("multiline data joins", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.message.content").String() != "a" {
			t.Fatalf("content = %s", got)
		}
	})
}

func TestCodeBuddyStreamTerminal(t *testing.T) {
	cases := map[string]struct {
		raw     string
		wantErr string
	}{
		"comments only": {": hi\n\n: ho\n\n", "empty stream"},
		"done only":     {"data: [DONE]\n\n", "DONE before finish"},
		"role only eof": {"data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n", "incomplete stream"},
		"truncated":     {"data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n", "truncated stream"},
		"malformed":     {"data: {oops\n\n", "malformed"},
		"business":      {string(readCodeBuddyStreamFixture(t, "business_error.sse")), "prompt is too long"},
	}
	for name, tc := range cases {
		_, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(tc.raw), "hy4-preview"))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	t.Run("data after done rejected", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"
		stream := NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview")
		for {
			_, err := stream.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("early error: %v", err)
			}
		}
		if _, err := stream.Next(); err == nil || !strings.Contains(err.Error(), "data after DONE") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("finish then clean eof accepted", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.finish_reason").String() != "stop" {
			t.Fatalf("finish = %s", got)
		}
	})
	t.Run("empty completion with finish accepted", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.message.content").String() != "" {
			t.Fatalf("content = %s", got)
		}
	})
	t.Run("midstream error keeps partial output", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"code\":11115,\"msg\":\"prompt is too long\"}\n\n"
		stream := NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview")
		chunk, err := stream.Next()
		if err != nil || !strings.Contains(string(chunk), "Hi") {
			t.Fatalf("chunk = %s err = %v", chunk, err)
		}
		if _, err := stream.Next(); err == nil {
			t.Fatal("midstream error swallowed")
		}
	})
	t.Run("usage not summed", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[],\"usage\":{\"total_tokens\":5}}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[],\"usage\":{\"total_tokens\":9}}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if value := gjson.GetBytes(got, "usage.total_tokens").Int(); value != 9 {
			t.Fatalf("total = %d", value)
		}
	})
	t.Run("local id when absent", func(t *testing.T) {
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(bytes.NewReader(readCodeBuddyStreamFixture(t, "reasoning.sse")), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if id := gjson.GetBytes(got, "id").String(); !strings.HasPrefix(id, "codebuddy-local-") {
			t.Fatalf("id = %q", id)
		}
		if gjson.GetBytes(got, "choices.0.message.content").String() != "42" {
			t.Fatalf("content = %s", got)
		}
		if gjson.GetBytes(got, "choices.0.message.reasoning_content").String() != "Synthetic reasoning fixture." {
			t.Fatalf("reasoning = %s", got)
		}
	})
}

func TestCodeBuddyStreamModelIdentity(t *testing.T) {
	raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"model\":\"other-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"evil\"}}]}\n\n"
	stream := NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview")
	if _, err := stream.Next(); err != nil {
		t.Fatalf("first: %v", err)
	}
	chunk, err := stream.Next()
	if err == nil || !strings.Contains(err.Error(), "model_mismatch") {
		t.Fatalf("chunk = %s err = %v", chunk, err)
	}
	if chunk != nil {
		t.Fatal("mismatched content emitted")
	}
}

func TestCodeBuddyStreamToolIdentity(t *testing.T) {
	t.Run("fragmented parallel tools", func(t *testing.T) {
		raw := readCodeBuddyStreamFixture(t, "tools.sse")
		for _, reader := range []io.Reader{bytes.NewReader(raw), &byteDribbleReader{raw: raw}} {
			got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(reader, "hy4-preview"))
			if err != nil {
				t.Fatalf("aggregate: %v", err)
			}
			calls := gjson.GetBytes(got, "choices.0.message.tool_calls")
			if len(calls.Array()) != 2 {
				t.Fatalf("calls = %s", got)
			}
			if calls.Get("0.id").String() != "call_a" || calls.Get("1.id").String() != "call_b" {
				t.Fatalf("ids = %s", got)
			}
			if calls.Get("0.function.arguments").String() != `{"path":"a.txt"}` || calls.Get("1.function.arguments").String() != `{}` {
				t.Fatalf("args = %s", got)
			}
			if gjson.GetBytes(got, "choices.0.finish_reason").String() != "tool_calls" {
				t.Fatalf("finish = %s", got)
			}
		}
	})
	t.Run("missing index normalized", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"solo\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		stream := NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview")
		chunk, err := stream.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 0 {
			t.Fatalf("index not normalized: %s", chunk)
		}
	})
	t.Run("ambiguous missing index rejected", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"b\",\"type\":\"function\",\"function\":{\"name\":\"g\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"x\"}}]}}]}\n\n"
		_, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid final args rejected", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{oops\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		_, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("message after deltas rejected", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		_, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err == nil || !strings.Contains(err.Error(), "cumulative message") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("length finish preserves partial args", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":1\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.finish_reason").String() != "length" {
			t.Fatalf("finish = %s", got)
		}
		if args := gjson.GetBytes(got, "choices.0.message.tool_calls.0.function.arguments").String(); args != `{"a":1` {
			t.Fatalf("args = %q", args)
		}
	})
	t.Run("terminal message normalized", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.message.content").String() != "Hi" {
			t.Fatalf("content = %s", got)
		}
	})
	t.Run("trailing content after args rejected", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}GARBAGE\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		_, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err == nil || !strings.Contains(err.Error(), "trailing content") {
			t.Fatalf("err = %v", err)
		}
		second := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{} {}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		if _, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(second), "hy4-preview")); err == nil {
			t.Fatal("second JSON object accepted")
		}
		padded := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":1}  \\t\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		if _, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(padded), "hy4-preview")); err != nil {
			t.Fatalf("whitespace-padded args rejected: %v", err)
		}
	})
	t.Run("aggregate emits index order", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"b\",\"type\":\"function\",\"function\":{\"name\":\"g\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if ids := gjson.GetBytes(got, "choices.0.message.tool_calls.#.id").String(); ids != `["a","b"]` {
			t.Fatalf("ids = %s (%s)", ids, got)
		}
	})
}

// codeBuddyCountingReader emits filler bytes without newlines so an oversize
// line never terminates.
type codeBuddyCountingReader struct {
	remaining int
	read      int
}

func (r *codeBuddyCountingReader) Read(out []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(out)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		out[i] = 'x'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

func TestCodeBuddyStreamBoundedLine(t *testing.T) {
	reader := &codeBuddyCountingReader{remaining: 32 << 20}
	_, err := NewCodeBuddyStream(reader, "hy4-preview").Next()
	if err == nil || !strings.Contains(err.Error(), "response_too_large") {
		t.Fatalf("err = %v", err)
	}
	if reader.read > 9<<20 {
		t.Fatalf("read %d bytes before 8 MiB rejection", reader.read)
	}
}

func TestCodeBuddyStreamSuccessCodes(t *testing.T) {
	t.Run("zero code with choices processed", func(t *testing.T) {
		raw := "data: {\"code\":0,\"msg\":\"ok\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.message.content").String() != "Hi" {
			t.Fatalf("content = %s", got)
		}
	})
	t.Run("string 200 code processed", func(t *testing.T) {
		raw := "data: {\"code\":\"200\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.message.content").String() != "Hi" {
			t.Fatalf("content = %s", got)
		}
	})
	t.Run("nonzero code still classified", func(t *testing.T) {
		raw := "data: {\"code\":11115,\"msg\":\"prompt is too long\"}\n\n"
		_, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err == nil || !strings.Contains(err.Error(), "prompt_too_long") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("null error with choices processed", func(t *testing.T) {
		raw := "data: {\"error\":null,\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		got, err := AggregateCodeBuddyStream(NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview"))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if gjson.GetBytes(got, "choices.0.message.content").String() != "Hi" {
			t.Fatalf("content = %s", got)
		}
	})
}

func TestCodeBuddyStreamNextReasoningPassthrough(t *testing.T) {
	raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think\"}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	stream := NewCodeBuddyStream(strings.NewReader(raw), "hy4-preview")
	chunk, err := stream.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if gjson.GetBytes(chunk, "choices.0.delta.reasoning_content").String() != "think" {
		t.Fatalf("reasoning not passed through: %s", chunk)
	}
}
