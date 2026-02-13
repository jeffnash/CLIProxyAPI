package executor

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSSELine_ReassemblesOversizedLine(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", 300_000)
	input := "data: " + large + "\n\n"
	reader := bufio.NewReaderSize(strings.NewReader(input), 256)

	first, err := readSSELine(reader)
	if err != nil {
		t.Fatalf("readSSELine first line: %v", err)
	}
	if got, want := string(first), "data: "+large; got != want {
		t.Fatalf("first line mismatch len=%d want=%d", len(got), len(want))
	}

	second, err := readSSELine(reader)
	if err != nil {
		t.Fatalf("readSSELine second line: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("expected empty SSE separator line, got %q", string(second))
	}

	_, err = readSSELine(reader)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after stream end, got %v", err)
	}
}

func TestReadSSELine_HandlesEOFWithoutTrailingNewline(t *testing.T) {
	t.Parallel()

	const input = "data: {\"type\":\"chunk\"}"
	reader := bufio.NewReaderSize(strings.NewReader(input), 16)

	line, err := readSSELine(reader)
	if err != nil {
		t.Fatalf("readSSELine: %v", err)
	}
	if got, want := string(line), input; got != want {
		t.Fatalf("line mismatch got=%q want=%q", got, want)
	}

	_, err = readSSELine(reader)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestReadSSELine_TrimsCRLFLineEnding(t *testing.T) {
	t.Parallel()

	reader := bufio.NewReaderSize(strings.NewReader("data: ok\r\n"), 16)
	line, err := readSSELine(reader)
	if err != nil {
		t.Fatalf("readSSELine: %v", err)
	}
	if got, want := string(line), "data: ok"; got != want {
		t.Fatalf("line mismatch got=%q want=%q", got, want)
	}
}

