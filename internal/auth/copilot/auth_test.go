package copilot

import "testing"

func resetCredentialIndexStateForTest() {
	githubCredentialIndexByID = map[string]int{}
	nextGitHubCredentialIndex = 1
}

func TestCredentialIndexForID_AssignsStableSequentialIndexes(t *testing.T) {
	resetCredentialIndexStateForTest()

	idx1 := credentialIndexForID("gh_aaa")
	if idx1 != 1 {
		t.Fatalf("first index = %d, want 1", idx1)
	}

	idx1Again := credentialIndexForID("gh_aaa")
	if idx1Again != 1 {
		t.Fatalf("reused index = %d, want 1", idx1Again)
	}

	idx2 := credentialIndexForID("gh_bbb")
	if idx2 != 2 {
		t.Fatalf("second unique index = %d, want 2", idx2)
	}
}

func TestCredentialIndexForID_EmptyIDReturnsZero(t *testing.T) {
	resetCredentialIndexStateForTest()
	if got := credentialIndexForID(""); got != 0 {
		t.Fatalf("empty id index = %d, want 0", got)
	}
	if got := credentialIndexForID("   "); got != 0 {
		t.Fatalf("blank id index = %d, want 0", got)
	}
}

func TestGitHubCredentialFingerprint_DeterministicAndTrimmed(t *testing.T) {
	first := githubCredentialFingerprint(" token-abc ")
	second := githubCredentialFingerprint("token-abc")
	if first == "" {
		t.Fatal("expected non-empty fingerprint")
	}
	if first != second {
		t.Fatalf("fingerprint mismatch: %q != %q", first, second)
	}
	if got := githubCredentialFingerprint("   "); got != "" {
		t.Fatalf("blank token fingerprint = %q, want empty", got)
	}
}
