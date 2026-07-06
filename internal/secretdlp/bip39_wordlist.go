package secretdlp

import (
	_ "embed"
	"strings"
)

// bip39_english.txt is the canonical 2048-word BIP39 English wordlist.
//
// It is intentionally not inlined as a Go array: a 2048-element literal is
// unreviewable and easy to corrupt. Fetch it at build/vendor time from the
// authoritative source and verify its digest:
//
//	curl -fsSL https://raw.githubusercontent.com/bitcoin/bips/master/bip-0039/english.txt \
//	  -o internal/secretdlp/bip39_english.txt
//	# SHA-256 must equal:
//	# 2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda
//
// The file is one lowercase word per line, 2048 lines. If it is absent the
// build fails through go:embed, which is preferable to silently disabling
// mnemonic detection.
//
//go:embed bip39_english.txt
var bip39WordlistRaw string

// bip39Wordlist maps each wordlist entry to its canonical 0-based index. The
// index is the word's 11-bit value and is required for checksum validation, so
// the map carries it rather than a bare membership set.
var bip39Wordlist = func() map[string]int {
	lines := strings.Split(strings.TrimSpace(bip39WordlistRaw), "\n")
	m := make(map[string]int, len(lines))
	for i, w := range lines {
		w = strings.TrimSpace(w)
		if w != "" {
			m[w] = i
		}
	}
	return m
}()
