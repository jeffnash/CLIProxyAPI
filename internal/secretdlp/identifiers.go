package secretdlp

import (
	"strconv"
	"strings"
	"unicode"
)

type IdentifierSet map[string]struct{}

const minSuppressiveIdentifierLength = 12

func harvestIdentifiers(doc *jsonDocument, pack PathPack) IdentifierSet {
	ids := make(IdentifierSet)
	if doc == nil || pack.RawOnly {
		return ids
	}
	harvestIdentifiersValue(doc.Root, nil, ids)
	return ids
}

func (s IdentifierSet) add(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	s[value] = struct{}{}
}

func (s IdentifierSet) containsOrEmbeds(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if _, ok := s[value]; ok && isSuppressiveIdentifier(value) {
		return true
	}
	for id := range s {
		if !isSuppressiveIdentifier(id) || len(id) > len(value) {
			continue
		}
		idx := strings.Index(value, id)
		for idx >= 0 {
			before := idx == 0 || isIdentifierBoundary(rune(value[idx-1]))
			afterIdx := idx + len(id)
			after := afterIdx == len(value) || isIdentifierBoundary(rune(value[afterIdx]))
			if before && after {
				return true
			}
			next := strings.Index(value[idx+1:], id)
			if next < 0 {
				break
			}
			idx += next + 1
		}
	}
	return false
}

func isSuppressiveIdentifier(value string) bool {
	return len(strings.TrimSpace(value)) >= minSuppressiveIdentifierLength
}

func harvestIdentifiersValue(value any, path []string, ids IdentifierSet) {
	switch v := value.(type) {
	case map[string]any:
		last := normalizeJSONPathKey(pathLast(path))
		if last == "parameters" || last == "input_schema" || last == "schema" || last == "properties" {
			for key := range v {
				ids.add(key)
			}
		}
		for key, child := range v {
			if shouldHarvestIdentifierString(path, key) {
				if s, ok := child.(string); ok {
					ids.add(s)
				}
			}
			harvestIdentifiersValue(child, append(path, key), ids)
		}
	case []any:
		for i, child := range v {
			harvestIdentifiersValue(child, append(path, indexPathPart(i)), ids)
		}
	case string:
		if shouldHarvestIdentifierLeaf(path) {
			ids.add(v)
		}
	}
}

func shouldHarvestIdentifierString(path []string, key string) bool {
	key = normalizeJSONPathKey(key)
	if key == "model" ||
		key == "id" ||
		key == "tool_call_id" ||
		key == "tool_use_id" ||
		key == "previous_response_id" {
		return true
	}
	if key == "name" && pathContains(path, "tools") {
		return true
	}
	if key == "name" && pathContains(path, "function") && pathContains(path, "tool_choice") {
		return true
	}
	return false
}

func shouldHarvestIdentifierLeaf(path []string) bool {
	last := normalizeJSONPathKey(pathLast(path))
	if last == "model" ||
		last == "id" ||
		last == "tool_call_id" ||
		last == "tool_use_id" ||
		last == "previous_response_id" {
		return true
	}
	if last == "tool_choice" {
		return true
	}
	parentPath := path
	if len(parentPath) > 0 {
		parentPath = parentPath[:len(parentPath)-1]
	}
	if last == "name" && pathContains(parentPath, "tools") {
		return true
	}
	return false
}

func pathContains(path []string, key string) bool {
	key = normalizeJSONPathKey(key)
	for _, part := range path {
		if normalizeJSONPathKey(part) == key {
			return true
		}
	}
	return false
}

func indexPathPart(i int) string {
	return strconv.Itoa(i)
}

func isIdentifierBoundary(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-')
}
