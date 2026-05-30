package executor

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
)

// protoDoubleValueField encodes a wire-type-1 (I64) field carrying an IEEE-754 double — the wire shape
// of google.protobuf.Value.number_value (field 2). decodeProtobufFields captures the 8 fixed bytes
// (no length prefix), which agentDecodeProtoValue reads via math.Float64frombits.
func protoDoubleValueField(fieldNumber int, d float64) []byte {
	tag := encodeVarint(uint64((fieldNumber << 3) | 1))
	var v [8]byte
	binary.LittleEndian.PutUint64(v[:], math.Float64bits(d))
	return append(tag, v[:]...)
}

// TestAgentDecodeProtoValue round-trips a google.protobuf.Value through every oneof arm
// (null / number / string / bool / struct / list, plus nesting) and asserts the Go types.
// This locks in the I64 numeric decode (the decodeProtobufFields wire-type-1 fix) against regression.
func TestAgentDecodeProtoValue(t *testing.T) {
	// null_value (field 1, varint) -> nil
	if got := agentDecodeProtoValue(protoVarintField(1, 0)); got != nil {
		t.Fatalf("null_value: want nil, got %#v", got)
	}
	// number_value (field 2, I64 double)
	if got := agentDecodeProtoValue(protoDoubleValueField(2, 3.5)); got != 3.5 {
		t.Fatalf("number_value 3.5: got %#v", got)
	}
	if got := agentDecodeProtoValue(protoDoubleValueField(2, -42)); got != float64(-42) {
		t.Fatalf("number_value -42: got %#v", got)
	}
	// string_value (field 3)
	if got := agentDecodeProtoValue(protoStringField(3, "hello")); got != "hello" {
		t.Fatalf("string_value: got %#v", got)
	}
	// bool_value (field 4, varint)
	if got := agentDecodeProtoValue(protoVarintField(4, 1)); got != true {
		t.Fatalf("bool_value true: got %#v", got)
	}
	if got := agentDecodeProtoValue(protoVarintField(4, 0)); got != false {
		t.Fatalf("bool_value false: got %#v", got)
	}

	// struct_value (field 5): Struct{fields}; each FieldsEntry = {key=1 string, value=2 Value}.
	mkEntry := func(key string, value []byte) []byte {
		return protoMessageField(1, protoMessage(protoStringField(1, key), protoMessageField(2, value)))
	}
	structVal := protoMessageField(5, protoMessage(
		mkEntry("a", protoStringField(3, "x")),
		mkEntry("n", protoDoubleValueField(2, 5)),
		mkEntry("ok", protoVarintField(4, 1)),
	))
	gotStruct := agentDecodeProtoValue(structVal)
	wantStruct := map[string]any{"a": "x", "n": float64(5), "ok": true}
	if !reflect.DeepEqual(gotStruct, wantStruct) {
		t.Fatalf("struct_value:\n want %#v\n got  %#v", wantStruct, gotStruct)
	}

	// list_value (field 6): ListValue{values: repeated Value at field 1}.
	listVal := protoMessageField(6, protoMessage(
		protoMessageField(1, protoDoubleValueField(2, 1)),
		protoMessageField(1, protoStringField(3, "two")),
		protoMessageField(1, protoVarintField(4, 1)),
	))
	gotList := agentDecodeProtoValue(listVal)
	wantList := []any{float64(1), "two", true}
	if !reflect.DeepEqual(gotList, wantList) {
		t.Fatalf("list_value:\n want %#v\n got  %#v", wantList, gotList)
	}

	// Nesting: a struct whose value is a list (exercises the recursion).
	nested := protoMessageField(5, protoMessage(mkEntry("items", listVal)))
	gotNested := agentDecodeProtoValue(nested)
	wantNested := map[string]any{"items": []any{float64(1), "two", true}}
	if !reflect.DeepEqual(gotNested, wantNested) {
		t.Fatalf("nested struct:\n want %#v\n got  %#v", wantNested, gotNested)
	}
}
