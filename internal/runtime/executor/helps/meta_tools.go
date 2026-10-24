package helps

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SanitizeMetaWebSearchTools strips unsupported fields from web_search tool definitions
// for Meta Muse upstreams. Specifically, Meta rejects search_content_types when tool type
// is "web_search" (it is only accepted on "web_search_preview" tools).
func SanitizeMetaWebSearchTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	var pathsToDelete []string
	tools.ForEach(func(index, tool gjson.Result) bool {
		if tool.Get("type").String() == "web_search" && tool.Get("search_content_types").Exists() {
			pathsToDelete = append(pathsToDelete, fmt.Sprintf("tools.%d.search_content_types", index.Int()))
		}
		if tool.Get("type").String() == "namespace" {
			subtools := tool.Get("tools")
			if subtools.Exists() && subtools.IsArray() {
				subtools.ForEach(func(subIndex, subtool gjson.Result) bool {
					if subtool.Get("type").String() == "web_search" && subtool.Get("search_content_types").Exists() {
						pathsToDelete = append(pathsToDelete, fmt.Sprintf("tools.%d.tools.%d.search_content_types", index.Int(), subIndex.Int()))
					}
					return true
				})
			}
		}
		return true
	})
	if len(pathsToDelete) == 0 {
		return body
	}
	out := body
	for _, p := range pathsToDelete {
		if updated, errDel := sjson.DeleteBytes(out, p); errDel == nil {
			out = updated
		}
	}
	return out
}

// metaToolSchemaKeywordDenylist lists JSON Schema keywords Meta rejects in
// tool schemas. Add a keyword here to cover future rejections of this class.
var metaToolSchemaKeywordDenylist = map[string]struct{}{
	"pattern": {},
}

// StripMetaToolSchemaKeywords removes denylisted keywords (initially
// "pattern") from every tool schema (input_schema / parameters) recursively.
// Missing or malformed tools/schemas are a no-op.
func StripMetaToolSchemaKeywords(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	out := body
	tools.ForEach(func(index, tool gjson.Result) bool {
		if !tool.IsObject() {
			return true
		}
		var decoded any
		if errUnmarshal := json.Unmarshal([]byte(tool.Raw), &decoded); errUnmarshal != nil {
			return true
		}
		if !stripMetaSchemasInValue(decoded) {
			return true
		}
		encoded, errMarshal := json.Marshal(decoded)
		if errMarshal != nil {
			return true
		}
		if updated, errSet := sjson.SetRawBytes(out, fmt.Sprintf("tools.%d", index.Int()), encoded); errSet == nil {
			out = updated
		}
		return true
	})
	return out
}

// stripMetaSchemasInValue finds input_schema/parameters subtrees (including
// namespace-nested tools) and strips denylisted keywords inside them.
func stripMetaSchemasInValue(value any) bool {
	changed := false
	switch node := value.(type) {
	case map[string]any:
		for _, schemaKey := range []string{"input_schema", "parameters"} {
			if schema, ok := node[schemaKey]; ok && stripMetaSchemaKeywords(schema) {
				changed = true
			}
		}
		for _, child := range node {
			if stripMetaSchemasInValue(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range node {
			if stripMetaSchemasInValue(child) {
				changed = true
			}
		}
	}
	return changed
}

// stripMetaSchemaKeywords deletes denylisted keys from a schema recursively.
// Keys under properties-like maps are argument names, not keywords, so only
// their values are descended into.
func stripMetaSchemaKeywords(value any) bool {
	changed := false
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if _, denied := metaToolSchemaKeywordDenylist[key]; denied {
				delete(node, key)
				changed = true
				continue
			}
			switch key {
			case "properties", "patternProperties", "$defs", "definitions":
				if named, ok := child.(map[string]any); ok {
					for _, subSchema := range named {
						if stripMetaSchemaKeywords(subSchema) {
							changed = true
						}
					}
				}
			default:
				if stripMetaSchemaKeywords(child) {
					changed = true
				}
			}
		}
	case []any:
		for _, child := range node {
			if stripMetaSchemaKeywords(child) {
				changed = true
			}
		}
	}
	return changed
}
