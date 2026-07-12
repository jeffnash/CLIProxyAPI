package turnprovenance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractSegments builds provider-neutral segments from a messages JSON array
// and optional top-level metadata. It understands generic cliproxy metadata only.
func ExtractSegments(messagesJSON []byte, topLevelMetadata gjson.Result) []Segment {
	arr := gjson.ParseBytes(messagesJSON)
	if !arr.IsArray() {
		// Allow callers to pass the full request body.
		arr = gjson.GetBytes(messagesJSON, "messages")
	}
	if !arr.IsArray() {
		return nil
	}
	out := make([]Segment, 0, len(arr.Array()))
	for i, m := range arr.Array() {
		role := Role(strings.ToLower(strings.TrimSpace(m.Get("role").String())))
		text := messageText(m)
		hasImages := messageHasImages(m)
		meta := mergeMessageMetadata(topLevelMetadata, m.Get("metadata"))
		clip := meta.Get("cliproxy")
		seg := Segment{
			ID:                 segmentID(i, m, text),
			OriginalIndex:      i,
			Role:               role,
			ContentDigest:      contentDigest(role, text, hasImages),
			ByteLength:         len(text),
			HasImages:          hasImages,
			TurnID:             firstNonEmpty(clip.Get("turn_id").String(), meta.Get("turn_id").String()),
			FragmentGroupID:    clip.Get("fragment_group_id").String(),
			ContinuationOf:     firstNonEmpty(clip.Get("continuation_of").String(), meta.Get("continuation_of").String()),
			DeclaredProvenance: Provenance(strings.ToLower(strings.TrimSpace(clip.Get("provenance").String()))),
			TextPreview:        preview(text, 120),
		}
		if clip.Get("fragment_index").Exists() {
			v := int(clip.Get("fragment_index").Int())
			seg.FragmentIndex = &v
		}
		if clip.Get("fragment_count").Exists() {
			v := int(clip.Get("fragment_count").Int())
			seg.FragmentCount = &v
		}
		out = append(out, seg)
	}
	return out
}

func mergeMessageMetadata(top, msg gjson.Result) gjson.Result {
	if msg.Exists() {
		return msg
	}
	return top
}

func messageText(m gjson.Result) string {
	content := m.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return strings.TrimSpace(m.Get("text").String())
	}
	var b strings.Builder
	for _, part := range content.Array() {
		typ := part.Get("type").String()
		switch typ {
		case "text", "":
			if t := part.Get("text").String(); t != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

func messageHasImages(m gjson.Result) bool {
	content := m.Get("content")
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		typ := part.Get("type").String()
		if typ == "image_url" || typ == "image" || part.Get("image_url").Exists() {
			return true
		}
	}
	return false
}

func contentDigest(role Role, text string, hasImages bool) string {
	sum := sha256.Sum256([]byte(string(role) + "\x00" + text + "\x00" + strconv.FormatBool(hasImages)))
	return hex.EncodeToString(sum[:16])
}

func segmentID(index int, m gjson.Result, text string) string {
	if id := strings.TrimSpace(m.Get("id").String()); id != "" {
		return id
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", index, text)))
	return "seg_" + hex.EncodeToString(sum[:8])
}

func preview(text string, n int) string {
	text = strings.TrimSpace(text)
	if n <= 0 || len(text) <= n {
		return text
	}
	return text[:n]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// FingerprintKey returns HMAC(tenant || conversation) material for store keys.
func FingerprintKey(secret []byte, tenantID, conversationID string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(tenantID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(conversationID))
	return hex.EncodeToString(mac.Sum(nil))
}

// SegmentFingerprint returns a keyed HMAC of a segment digest for durable storage.
func SegmentFingerprint(secret []byte, tenantID, conversationID, contentDigest string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(tenantID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(conversationID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(contentDigest))
	return hex.EncodeToString(mac.Sum(nil))
}
