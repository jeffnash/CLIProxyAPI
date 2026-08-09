package secretdlp

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
)

type identifierKind uint8

const (
	// General identifiers include tool/schema names. Credential-shaped findings
	// may still override them because callers sometimes paste real keys there.
	identifierKindGeneral identifierKind = 1 << iota
	// Protocol identifiers come from semantic id/ref fields. Ambiguous token-shape
	// heuristics must not mutate them or any repeated copies in model-visible text.
	identifierKindProtocol
)

type IdentifierSet map[string]identifierKind

const minSuppressiveIdentifierLength = 12

const maxEmbeddedJSONIdentifierDepth = 4

func harvestIdentifiers(doc *jsonDocument, pack PathPack) IdentifierSet {
	ids := make(IdentifierSet)
	if doc == nil || pack.RawOnly {
		return ids
	}
	harvestIdentifiersValue(doc.Root, nil, ids)
	return ids
}

func (s IdentifierSet) add(value string) {
	s.addKind(value, identifierKindGeneral)
}

func (s IdentifierSet) addProtocol(value string) {
	s.addKind(value, identifierKindProtocol)
}

func (s IdentifierSet) addKind(value string, kind identifierKind) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	s[value] |= kind
}

func (s IdentifierSet) containsOrEmbeds(value string) bool {
	return s.containsOrEmbedsKind(value, identifierKindGeneral|identifierKindProtocol)
}

func (s IdentifierSet) containsProtocolOrEmbeds(value string) bool {
	return s.containsOrEmbedsKind(value, identifierKindProtocol)
}

func (s IdentifierSet) containsOrEmbedsKind(value string, kind identifierKind) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if storedKind, ok := s[value]; ok && storedKind&kind != 0 && isSuppressiveIdentifier(value) {
		return true
	}
	for id, storedKind := range s {
		if storedKind&kind == 0 || !isSuppressiveIdentifier(id) || len(id) > len(value) {
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
	harvestIdentifiersValueDepth(value, path, ids, 0)
}

func harvestIdentifiersValueDepth(value any, path []string, ids IdentifierSet, embeddedDepth int) {
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
					if isProtocolIdentifierKey(key) || normalizeJSONPathKey(key) == "model" {
						ids.addProtocol(s)
					} else {
						ids.add(s)
					}
				}
			}
			harvestIdentifiersValueDepth(child, append(path, key), ids, embeddedDepth)
		}
	case []any:
		for i, child := range v {
			harvestIdentifiersValueDepth(child, append(path, indexPathPart(i)), ids, embeddedDepth)
		}
	case string:
		if isProtocolIdentifierPath(path) {
			ids.addProtocol(v)
		} else if shouldHarvestIdentifierLeaf(path) {
			ids.add(v)
		}
		harvestEmbeddedJSONIdentifiers(v, path, ids, embeddedDepth)
	}
}

func harvestEmbeddedJSONIdentifiers(value string, path []string, ids IdentifierSet, embeddedDepth int) {
	if embeddedDepth >= maxEmbeddedJSONIdentifierDepth {
		return
	}
	value = strings.TrimSpace(value)
	if len(value) < 2 || (value[0] != '{' && value[0] != '[') || !json.Valid([]byte(value)) {
		return
	}

	var nested any
	if err := json.Unmarshal([]byte(value), &nested); err != nil {
		return
	}
	// Tool results and tool-call arguments frequently wrap a second JSON document
	// in a string. Preserve its semantic ids just as if they were top-level JSON.
	harvestIdentifiersValueDepth(nested, path, ids, embeddedDepth+1)
}

func shouldHarvestIdentifierString(path []string, key string) bool {
	key = normalizeJSONPathKey(key)
	if key == "model" || isProtocolIdentifierKey(key) {
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
	if isProtocolIdentifierPath(path) {
		return true
	}
	last := normalizeJSONPathKey(pathLast(path))
	if last == "model" {
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

func isProtocolIdentifierPath(path []string) bool {
	last := normalizeJSONPathKey(pathLast(path))
	if isProtocolIdentifierKey(last) {
		return true
	}
	if _, err := strconv.Atoi(last); err != nil || len(path) < 2 {
		return false
	}
	return isProtocolIdentifierKey(path[len(path)-2])
}

func isProtocolIdentifierKey(key string) bool {
	camelCaseIdentifier := hasCamelCaseProtocolIdentifierSuffix(strings.TrimSpace(key))
	key = normalizeJSONPathKey(key)
	return camelCaseIdentifier ||
		key == "id" || key == "ids" || key == "identifier" || key == "identifiers" ||
		key == "ref" || key == "refs" || key == "reference" || key == "references" ||
		key == "uuid" || key == "uuids" || key == "guid" || key == "guids" ||
		key == "cursor" ||
		strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids") ||
		strings.HasSuffix(key, "_identifier") || strings.HasSuffix(key, "_identifiers") ||
		strings.HasSuffix(key, "_ref") || strings.HasSuffix(key, "_refs") ||
		strings.HasSuffix(key, "_reference") || strings.HasSuffix(key, "_references") ||
		strings.HasSuffix(key, "_uuid") || strings.HasSuffix(key, "_uuids") ||
		strings.HasSuffix(key, "_guid") || strings.HasSuffix(key, "_guids") ||
		strings.HasSuffix(key, "_cursor")
}

func hasCamelCaseProtocolIdentifierSuffix(key string) bool {
	for _, suffix := range []string{
		"Id", "Ids", "ID", "IDs", "Identifier", "Identifiers",
		"Ref", "Refs", "Reference", "References",
		"Uuid", "Uuids", "UUID", "UUIDs", "Guid", "Guids", "GUID", "GUIDs",
		"Cursor",
	} {
		if len(key) <= len(suffix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		previous := rune(key[len(key)-len(suffix)-1])
		if unicode.IsLower(previous) || unicode.IsDigit(previous) {
			return true
		}
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
