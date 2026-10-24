package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeMetaWebSearchTools_StripsSearchContentTypesFromWebSearch(t *testing.T) {
	input := []byte(`{
		"model": "muse-spark-1.3",
		"tools": [
			{
				"type": "function",
				"name": "lookup",
				"parameters": {"type": "object"}
			},
			{
				"type": "web_search",
				"external_web_access": true,
				"search_content_types": ["text", "image"]
			},
			{
				"type": "web_search_preview",
				"search_content_types": ["text", "image"]
			},
			{
				"type": "namespace",
				"name": "search_group",
				"tools": [
					{
						"type": "web_search",
						"search_content_types": ["text"]
					}
				]
			}
		]
	}`)

	got := SanitizeMetaWebSearchTools(input)

	// web_search at index 1 must have search_content_types stripped
	tool1 := gjson.GetBytes(got, "tools.1")
	if tool1.Get("type").String() != "web_search" {
		t.Fatalf("tools.1 type = %q, want web_search", tool1.Get("type").String())
	}
	if tool1.Get("search_content_types").Exists() {
		t.Fatalf("tools.1 still has search_content_types: %s", tool1.Raw)
	}
	if !tool1.Get("external_web_access").Bool() {
		t.Fatalf("tools.1 external_web_access was lost: %s", tool1.Raw)
	}

	// web_search_preview at index 2 must retain search_content_types
	tool2 := gjson.GetBytes(got, "tools.2")
	if tool2.Get("type").String() != "web_search_preview" {
		t.Fatalf("tools.2 type = %q, want web_search_preview", tool2.Get("type").String())
	}
	if !tool2.Get("search_content_types").Exists() {
		t.Fatalf("tools.2 search_content_types missing: %s", tool2.Raw)
	}

	// nested web_search in namespace at index 3 must have search_content_types stripped
	nestedTool := gjson.GetBytes(got, "tools.3.tools.0")
	if nestedTool.Get("type").String() != "web_search" {
		t.Fatalf("tools.3.tools.0 type = %q, want web_search", nestedTool.Get("type").String())
	}
	if nestedTool.Get("search_content_types").Exists() {
		t.Fatalf("tools.3.tools.0 still has search_content_types: %s", nestedTool.Raw)
	}

	// function tool at index 0 must be unchanged
	tool0 := gjson.GetBytes(got, "tools.0")
	if tool0.Get("name").String() != "lookup" {
		t.Fatalf("tools.0 name = %q, want lookup", tool0.Get("name").String())
	}
}

func TestStripMetaToolSchemaKeywords_RemovesPatternRecursively(t *testing.T) {
	input := []byte(`{
		"model": "muse-wonder-full-1.0-contributor",
		"input": "hello",
		"tools": [
			{
				"name": "lookup",
				"description": "look up text",
				"input_schema": {
					"type": "object",
					"properties": {
						"query": {"maxLength":1024,"minLength":1,"pattern":"^[^\\0]*$","type":"string"},
						"options": {
							"type": "object",
							"properties": {
								"filter": {"type": "string", "pattern": "^[a-z]+$", "enum": ["a", "b"]}
							},
							"anyOf": [
								{"type": "string", "pattern": "^x", "description": "starts with x"},
								{"type": "integer"}
							]
						},
						"pattern": {"type": "string", "description": "arg named pattern"}
					},
					"required": ["query"]
				}
			},
			{
				"type": "function",
				"name": "search",
				"parameters": {
					"type": "object",
					"properties": {
						"items": {"type": "array", "items": {"type": "string", "pattern": "^y"}}
					}
				}
			}
		]
	}`)

	got := StripMetaToolSchemaKeywords(input)

	assertNoPattern := func(path string) {
		t.Helper()
		if gjson.GetBytes(got, path).Exists() {
			t.Errorf("%s still exists in %s", path, got)
		}
	}
	assertNoPattern("tools.0.input_schema.properties.query.pattern")
	assertNoPattern("tools.0.input_schema.properties.options.properties.filter.pattern")
	assertNoPattern("tools.0.input_schema.properties.options.anyOf.0.pattern")
	assertNoPattern("tools.1.parameters.properties.items.items.pattern")

	// Every other key/value is unchanged.
	query := gjson.GetBytes(got, "tools.0.input_schema.properties.query")
	if query.Get("type").String() != "string" || query.Get("minLength").Int() != 1 || query.Get("maxLength").Int() != 1024 {
		t.Errorf("query schema changed unexpectedly: %s", query.Raw)
	}
	filter := gjson.GetBytes(got, "tools.0.input_schema.properties.options.properties.filter")
	if filter.Get("type").String() != "string" || len(filter.Get("enum").Array()) != 2 {
		t.Errorf("filter schema changed unexpectedly: %s", filter.Raw)
	}
	if d := gjson.GetBytes(got, "tools.0.input_schema.properties.options.anyOf.0.description").String(); d != "starts with x" {
		t.Errorf("anyOf description changed: %q", d)
	}
	// A property NAMED pattern is an argument definition, not the keyword.
	namedArg := gjson.GetBytes(got, "tools.0.input_schema.properties.pattern")
	if !namedArg.Exists() || namedArg.Get("type").String() != "string" {
		t.Errorf("argument named pattern was removed: %s", got)
	}
	if m := gjson.GetBytes(got, "model").String(); m != "muse-wonder-full-1.0-contributor" {
		t.Errorf("model changed: %q", m)
	}
}

func TestStripMetaToolSchemaKeywords_NoToolsOrMalformedIsNoOp(t *testing.T) {
	if got := StripMetaToolSchemaKeywords(nil); len(got) != 0 {
		t.Fatalf("expected empty output for nil input, got %s", got)
	}
	for _, payload := range [][]byte{
		[]byte(`{"model":"muse-spark-1.3","input":"hello"}`),
		[]byte(`{"tools":"not-an-array"}`),
		[]byte(`{"tools":[{"name":"x","input_schema":"not-an-object"}]}`),
		[]byte(`not json`),
	} {
		if got := StripMetaToolSchemaKeywords(payload); string(got) != string(payload) {
			t.Fatalf("expected unmodified payload %s, got %s", payload, got)
		}
	}
}

func TestSanitizeMetaWebSearchTools_NoToolsOrEmpty(t *testing.T) {
	if got := SanitizeMetaWebSearchTools(nil); len(got) != 0 {
		t.Fatalf("expected empty output for nil input, got %s", got)
	}

	payloadWithoutTools := []byte(`{"model":"muse-spark-1.3","input":"hello"}`)
	if got := SanitizeMetaWebSearchTools(payloadWithoutTools); string(got) != string(payloadWithoutTools) {
		t.Fatalf("expected unmodified payload, got %s", got)
	}
}
