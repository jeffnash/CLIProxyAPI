package executor

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/turnprovenance"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	composerProvenanceOnce     sync.Once
	composerProvenanceResolver *turnprovenance.Resolver
)

func composerProvenance() *turnprovenance.Resolver {
	composerProvenanceOnce.Do(func() {
		// Never fall back to a public/dev constant: forgeable resolution tokens
		// are worse than unsigned clarification. TokenSecret can safely derive a
		// purpose-separated key from the stable Composer lineage secret.
		secret, _ := turnprovenance.TokenSecret()
		composerProvenanceResolver = &turnprovenance.Resolver{
			Store:  turnprovenance.NewMemoryStore(secret, 3, 64),
			Secret: secret,
		}
	})
	return composerProvenanceResolver
}

func composerConversationIDFromPayload(payload []byte) string {
	for _, path := range []string{
		"metadata.cliproxy.conversation_id",
		"metadata.conversation_id",
		"metadata.conversationId",
		"metadata.session_id",
		"metadata.sessionId",
		"conversation_id",
	} {
		if v := strings.TrimSpace(gjson.GetBytes(payload, path).String()); v != "" {
			return v
		}
	}
	userID := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
	if strings.HasPrefix(userID, "{") {
		for _, k := range []string{"session_id", "conversation_id", "thread_id"} {
			if v := strings.TrimSpace(gjson.Get(userID, k).String()); v != "" {
				return v
			}
		}
	}
	return ""
}

// resolveComposerProvenance runs the provider-neutral resolver before upstream send.
// Ambiguous turns return ClarificationError (HTTP 422). Resolved standing context is
// rewritten into system-role standing records without merging novel user intent.
func resolveComposerProvenance(tenantID string, oai []byte, opts cliproxyexecutor.Options, toolCapable bool) ([]byte, *turnprovenance.ClarificationError) {
	messagesRaw := gjson.GetBytes(oai, "messages").Raw
	if messagesRaw == "" {
		return oai, nil
	}
	topMeta := gjson.GetBytes(opts.OriginalRequest, "metadata")
	if !topMeta.Exists() {
		topMeta = gjson.GetBytes(oai, "metadata")
	}
	segments := turnprovenance.ExtractSegments([]byte(messagesRaw), topMeta)
	if len(segments) == 0 {
		return oai, nil
	}

	// Keep the legacy structural detector as an input signal, not final policy.
	_ = composerAmbiguousTrailingUserSegments(gjson.Parse(messagesRaw).Array())

	conversationID := composerConversationIDFromPayload(opts.OriginalRequest)
	if conversationID == "" {
		conversationID = composerConversationIDFromPayload(oai)
	}
	invocationID := composerInvocationID(oai, opts)
	in := turnprovenance.Input{
		TenantID:       tenantID,
		ConversationID: conversationID,
		InvocationID:   invocationID,
		Segments:       segments,
		ToolCapable:    toolCapable,
	}
	r := composerProvenance()
	digest := turnprovenance.RequestDigest(oai)
	if req, ok := extractResolutionRequest(opts, oai); ok {
		decision, cerr := applySignedResolution(r, in, digest, req)
		if cerr != nil {
			return oai, cerr
		}
		rewritten, rewrittenOK := rewriteStandingContext(oai, segments, decision)
		if !rewrittenOK {
			return oai, nil
		}
		return rewritten, nil
	}
	decision := r.Resolve(in)
	if decision.ClarificationRequired() {
		// Clients that do not advertise the clarification protocol cannot act on
		// a 422. Preserve their request losslessly and let the provider interpret
		// the original ordering instead of guessing or returning a terminal error.
		if !cliproxyexecutor.HasCapability(opts.Headers, cliproxyexecutor.CapabilityProvenanceClarificationV1) {
			return oai, nil
		}
		return oai, r.Clarify(in, decision, digest)
	}

	rewritten, ok := rewriteStandingContext(oai, segments, decision)
	if !ok {
		return oai, nil
	}
	return rewritten, nil
}

func composerCompletionObservable(stopReason string) bool {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "", "end_turn", "stop", "completed":
		return true
	default:
		return false
	}
}

// observeComposerCompleted trains recurrence only after a COMPLETED turn.
func observeComposerCompleted(tenantID string, oai []byte, opts cliproxyexecutor.Options, stopReason string) {
	if !composerCompletionObservable(stopReason) {
		return
	}
	messagesRaw := gjson.GetBytes(oai, "messages").Raw
	if messagesRaw == "" {
		return
	}
	topMeta := gjson.GetBytes(opts.OriginalRequest, "metadata")
	if !topMeta.Exists() {
		topMeta = gjson.GetBytes(oai, "metadata")
	}
	segments := turnprovenance.ExtractSegments([]byte(messagesRaw), topMeta)
	if len(segments) == 0 {
		return
	}
	conversationID := composerConversationIDFromPayload(opts.OriginalRequest)
	if conversationID == "" {
		conversationID = composerConversationIDFromPayload(oai)
	}
	composerProvenance().ObserveCompletedTurn(turnprovenance.Input{
		TenantID:       tenantID,
		ConversationID: conversationID,
		InvocationID:   composerInvocationID(oai, opts),
		Segments:       segments,
	})
}

func extractResolutionRequest(opts cliproxyexecutor.Options, oai []byte) (turnprovenance.ResolutionRequest, bool) {
	sources := [][]byte{opts.OriginalRequest, oai}
	for _, src := range sources {
		if len(src) == 0 {
			continue
		}
		root := gjson.ParseBytes(src)
		meta := root.Get("metadata.cliproxy")
		if !meta.Exists() {
			meta = root.Get("metadata")
		}
		token := strings.TrimSpace(meta.Get("resolution_token").String())
		if token == "" {
			token = strings.TrimSpace(root.Get("resolution_token").String())
		}
		if token == "" {
			continue
		}
		req := turnprovenance.ResolutionRequest{ResolutionToken: token}
		for _, id := range meta.Get("current_segments").Array() {
			if v := strings.TrimSpace(id.String()); v != "" {
				req.CurrentSegments = append(req.CurrentSegments, v)
			}
		}
		for _, id := range meta.Get("standing_segments").Array() {
			if v := strings.TrimSpace(id.String()); v != "" {
				req.StandingSegments = append(req.StandingSegments, v)
			}
		}
		for _, group := range meta.Get("merge_groups").Array() {
			var ids []string
			for _, id := range group.Array() {
				if v := strings.TrimSpace(id.String()); v != "" {
					ids = append(ids, v)
				}
			}
			if len(ids) > 0 {
				req.MergeGroups = append(req.MergeGroups, ids)
			}
		}
		return req, true
	}
	return turnprovenance.ResolutionRequest{}, false
}

func applySignedResolution(r *turnprovenance.Resolver, in turnprovenance.Input, digest string, req turnprovenance.ResolutionRequest) (turnprovenance.Decision, *turnprovenance.ClarificationError) {
	if r == nil {
		return turnprovenance.Decision{}, &turnprovenance.ClarificationError{
			InvocationID: in.InvocationID,
			Message:      "provenance resolver unavailable",
		}
	}
	secret := r.Secret
	if len(secret) == 0 {
		return turnprovenance.Decision{}, &turnprovenance.ClarificationError{
			InvocationID: in.InvocationID,
			Message:      "signed provenance resolution requires CLIPROXY_TURN_PROVENANCE_SECRET",
		}
	}
	payload, err := turnprovenance.VerifyResolutionToken(secret, req.ResolutionToken, in.TenantID, in.ConversationID, in.InvocationID, digest, in.Now)
	if err != nil {
		return turnprovenance.Decision{}, &turnprovenance.ClarificationError{
			InvocationID: in.InvocationID,
			Message:      "invalid provenance resolution token: " + err.Error(),
		}
	}
	decision, err := turnprovenance.ApplyResolution(payload, req)
	if err != nil {
		return turnprovenance.Decision{}, &turnprovenance.ClarificationError{
			InvocationID: in.InvocationID,
			Message:      "invalid provenance resolution selection: " + err.Error(),
		}
	}
	return decision, nil
}

func rewriteStandingContext(oai []byte, segments []turnprovenance.Segment, decision turnprovenance.Decision) ([]byte, bool) {
	switch decision.Kind {
	case turnprovenance.DecisionResolvedRecurrence, turnprovenance.DecisionResolvedExplicit:
		return rewriteStandingRoles(oai, segments, decision)
	case turnprovenance.DecisionResolvedFragments:
		return rewriteFragmentGroups(oai, segments, decision)
	default:
		return oai, false
	}
}

func rewriteStandingRoles(oai []byte, segments []turnprovenance.Segment, decision turnprovenance.Decision) ([]byte, bool) {
	standing := map[string]bool{}
	for _, id := range decision.StandingSegments {
		standing[id] = true
	}
	current := map[string]bool{}
	for _, id := range decision.CurrentSegments {
		current[id] = true
	}
	if len(standing) == 0 || len(current) == 0 {
		return oai, false
	}

	msgs := gjson.GetBytes(oai, "messages")
	if !msgs.IsArray() || len(msgs.Array()) != len(segments) {
		return oai, false
	}
	out := make([]any, 0, len(msgs.Array()))
	changed := false
	for i, m := range msgs.Array() {
		seg := segments[i]
		role := strings.ToLower(m.Get("role").String())
		if role == "user" && standing[seg.ID] && !current[seg.ID] {
			out = append(out, map[string]any{
				"role":    "system",
				"content": m.Get("content").Value(),
				"metadata": map[string]any{
					"cliproxy": map[string]any{
						"provenance": "standing",
						"source":     "turnprovenance",
					},
				},
			})
			changed = true
			continue
		}
		var raw any
		if err := json.Unmarshal([]byte(m.Raw), &raw); err != nil {
			out = append(out, m.Value())
			continue
		}
		out = append(out, raw)
	}
	if !changed {
		return oai, false
	}
	return setMessages(oai, out)
}

// rewriteFragmentGroups deterministically merges validated fragment groups into
// one current user record per group while rewriting standing users to system.
func rewriteFragmentGroups(oai []byte, segments []turnprovenance.Segment, decision turnprovenance.Decision) ([]byte, bool) {
	msgs := gjson.GetBytes(oai, "messages")
	if !msgs.IsArray() || len(msgs.Array()) != len(segments) {
		return oai, false
	}
	msgByID := map[string]gjson.Result{}
	segByID := map[string]turnprovenance.Segment{}
	for i, seg := range segments {
		msgByID[seg.ID] = msgs.Array()[i]
		segByID[seg.ID] = seg
	}

	standing := map[string]bool{}
	for _, id := range decision.StandingSegments {
		standing[id] = true
	}
	mergedIDs := map[string]bool{}
	for _, group := range decision.MergeGroups {
		for _, id := range group {
			mergedIDs[id] = true
		}
	}
	if len(mergedIDs) == 0 {
		return oai, false
	}

	emittedGroup := map[string]bool{}
	groupKey := map[string]string{}
	for gi, group := range decision.MergeGroups {
		key := "g:" + strings.Join(group, ",")
		_ = gi
		for _, id := range group {
			groupKey[id] = key
		}
	}

	out := make([]any, 0, len(segments))
	changed := false
	for _, seg := range segments {
		if mergedIDs[seg.ID] {
			key := groupKey[seg.ID]
			if emittedGroup[key] {
				continue
			}
			emittedGroup[key] = true
			var group []string
			for _, g := range decision.MergeGroups {
				if groupKey[g[0]] == key {
					group = g
					break
				}
			}
			merged, ok := mergeFragmentContents(group, msgByID)
			if !ok {
				return oai, false
			}
			out = append(out, merged)
			changed = true
			continue
		}
		role := strings.ToLower(msgByID[seg.ID].Get("role").String())
		if role == "user" && standing[seg.ID] {
			out = append(out, map[string]any{
				"role":    "system",
				"content": msgByID[seg.ID].Get("content").Value(),
				"metadata": map[string]any{
					"cliproxy": map[string]any{
						"provenance": "standing",
						"source":     "turnprovenance",
					},
				},
			})
			changed = true
			continue
		}
		var raw any
		if err := json.Unmarshal([]byte(msgByID[seg.ID].Raw), &raw); err != nil {
			out = append(out, msgByID[seg.ID].Value())
			continue
		}
		out = append(out, raw)
	}
	if !changed {
		return oai, false
	}
	return setMessages(oai, out)
}

func mergeFragmentContents(group []string, msgByID map[string]gjson.Result) (map[string]any, bool) {
	if len(group) == 0 {
		return nil, false
	}
	parts := make([]any, 0, len(group))
	var textParts []string
	allText := true
	for _, id := range group {
		m, ok := msgByID[id]
		if !ok {
			return nil, false
		}
		content := m.Get("content")
		if content.Type == gjson.String {
			textParts = append(textParts, content.String())
			parts = append(parts, content.String())
			continue
		}
		allText = false
		if content.IsArray() {
			for _, part := range content.Array() {
				var raw any
				if err := json.Unmarshal([]byte(part.Raw), &raw); err != nil {
					parts = append(parts, part.Value())
				} else {
					parts = append(parts, raw)
				}
			}
			continue
		}
		parts = append(parts, content.Value())
	}
	obj := map[string]any{
		"role": "user",
		"metadata": map[string]any{
			"cliproxy": map[string]any{
				"provenance": "current",
				"source":     "turnprovenance_fragments",
				"merged_ids": append([]string(nil), group...),
			},
		},
	}
	if allText {
		obj["content"] = strings.Join(textParts, "")
	} else {
		obj["content"] = parts
	}
	return obj, true
}

func setMessages(oai []byte, out []any) ([]byte, bool) {
	encoded, err := json.Marshal(out)
	if err != nil {
		return oai, false
	}
	next, err := sjson.SetRawBytes(oai, "messages", encoded)
	if err != nil {
		return oai, false
	}
	return next, true
}
