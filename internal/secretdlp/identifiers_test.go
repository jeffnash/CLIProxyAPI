package secretdlp

import "testing"

func TestProtocolIdentifierKeyRecognizesCommonSerializationStyles(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"id", "task_id", "task-id", "task id", "taskId", "taskID",
		"evidence_refs", "evidenceReferences", "request_uuid", "traceGUID",
		"cursor", "next_cursor", "nextCursor", "identifiers",
	} {
		if !isProtocolIdentifierKey(key) {
			t.Errorf("isProtocolIdentifierKey(%q) = false, want true", key)
		}
	}
}

func TestProtocolIdentifierKeyRejectsCredentialAndIncidentalSuffixKeys(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"api_key", "token", "password", "secret", "nonce", "valid", "grid", "name",
	} {
		if isProtocolIdentifierKey(key) {
			t.Errorf("isProtocolIdentifierKey(%q) = true, want false", key)
		}
	}
}
