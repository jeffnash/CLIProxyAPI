package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	grokauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/grok"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ConvertOpenAIRequestToGrok(modelName string, inputRawJSON []byte, stream bool) []byte {
	// Streaming is handled by the executor; Grok payload shape is the same for stream/non-stream.
	rawJSON := bytes.Clone(inputRawJSON)

	cfg, ok := grokauth.GrokModels[modelName]
	if !ok {
		log.Warnf("grok translator: unknown model %q; returning original payload for upstream handling", modelName)
		return rawJSON
	}

	message, plainText, imageAttachments := extractOpenAIContent(rawJSON)
	_ = plainText
	imageCount := len(imageAttachments)
	hasImages := imageCount > 0
	toggles := defaultGrokRequestToggles(hasImages)

	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "temporary", true)
	out, _ = sjson.SetBytes(out, "modelName", cfg.GrokModel)
	out, _ = sjson.SetBytes(out, "message", message)
	out, _ = sjson.SetBytes(out, "fileAttachments", []string{})
	out, _ = sjson.SetBytes(out, "imageAttachments", imageAttachments)
	out, _ = sjson.SetBytes(out, "disableSearch", toggles.disableSearch)
	out, _ = sjson.SetBytes(out, "enableImageGeneration", toggles.enableImageGeneration)
	out, _ = sjson.SetBytes(out, "returnImageBytes", toggles.returnImageBytes)
	out, _ = sjson.SetBytes(out, "enableImageStreaming", toggles.enableImageStreaming)
	out, _ = sjson.SetBytes(out, "imageGenerationCount", toggles.imageGenerationCount)
	out, _ = sjson.SetBytes(out, "forceConcise", toggles.forceConcise)
	out, _ = sjson.SetBytes(out, "toolOverrides", map[string]interface{}{})
	out, _ = sjson.SetBytes(out, "enableSideBySide", toggles.enableSideBySide)
	out, _ = sjson.SetBytes(out, "sendFinalMetadata", toggles.sendFinalMetadata)
	out, _ = sjson.SetBytes(out, "isReasoning", toggles.isReasoning)
	out, _ = sjson.SetBytes(out, "webpageUrls", []string{})
	out, _ = sjson.SetBytes(out, "disableTextFollowUps", toggles.disableTextFollowUps)
	out, _ = sjson.SetBytes(out, "returnRawGrokInXaiRequest", toggles.returnRawGrokInXaiRequest)
	out, _ = sjson.SetBytes(out, "responseMetadata", map[string]any{"requestModelDetails": map[string]string{"modelId": cfg.GrokModel}})
	out, _ = sjson.SetBytes(out, "disableMemory", toggles.disableMemory)
	out, _ = sjson.SetBytes(out, "forceSideBySide", toggles.forceSideBySide)
	out, _ = sjson.SetBytes(out, "modelMode", cfg.ModelMode)
	out, _ = sjson.SetBytes(out, "isAsyncChat", toggles.isAsyncChat)

	// OpenAI parameters (temperature, max_tokens, top_p) are intentionally not mapped here.
	// Grok controls these via model configuration; executors should enforce or translate
	// these settings if/when Grok adds equivalent per-request controls.

	return out
}

func extractOpenAIContent(rawJSON []byte) (messageWithRoles string, plainText string, imageAttachments []map[string]string) {
	root := gjson.ParseBytes(rawJSON)

	var contentBuilder strings.Builder
	var plainBuilder strings.Builder
	imageAttachments = make([]map[string]string, 0)

	// Extract tools and build tool instruction if present
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		toolInstruction := buildToolInstruction(tools)
		if toolInstruction != "" {
			contentBuilder.WriteString("system: ")
			contentBuilder.WriteString(toolInstruction)
			contentBuilder.WriteString("\n")
		}
	}

	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := msg.Get("content")

			// Handle tool results from previous tool calls
			if role == "tool" {
				contentBuilder.WriteString(formatToolResult(msg.Get("tool_call_id").String(), content))
				contentBuilder.WriteString("\n")
				return true
			}

			// Handle assistant messages with tool_calls
			if role == "assistant" {
				if toolCalls := msg.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
					contentBuilder.WriteString("assistant: ")
					toolCalls.ForEach(func(_, tc gjson.Result) bool {
						callID := tc.Get("id").String()
						funcName := tc.Get("function.name").String()
						funcArgs := tc.Get("function.arguments").String()
						contentBuilder.WriteString(formatToolCall(funcName, callID, funcArgs))
						return true
					})
					contentBuilder.WriteString("\n")
				}
				// Also include any text content
				if content.Type == gjson.String && content.String() != "" {
					contentBuilder.WriteString("assistant: ")
					contentBuilder.WriteString(content.String())
					contentBuilder.WriteString("\n")
				}
				return true
			}

			if content.Type == gjson.String {
				text := content.String()
				if text != "" {
					contentBuilder.WriteString(role)
					contentBuilder.WriteString(": ")
					contentBuilder.WriteString(text)
					contentBuilder.WriteString("\n")
					plainBuilder.WriteString(text)
				}
			} else if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					partType := part.Get("type").String()
					switch partType {
					case "text":
						text := part.Get("text").String()
						if text != "" {
							contentBuilder.WriteString(role)
							contentBuilder.WriteString(": ")
							contentBuilder.WriteString(text)
							contentBuilder.WriteString("\n")
							plainBuilder.WriteString(text)
						}
					case "image_url":
						if imageURL := part.Get("image_url.url").String(); imageURL != "" {
							imageAttachments = append(imageAttachments, map[string]string{"url": imageURL})
						}
					default:
						if imageURL := part.Get("image_url.url").String(); imageURL != "" {
							imageAttachments = append(imageAttachments, map[string]string{"url": imageURL})
						}
					}
					return true
				})
			}
			return true
		})
	}

	// Append end-of-context reminder if tools are present (combats recency bias)
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		contentBuilder.WriteString("\nsystem: [REMINDER BEFORE YOU RESPOND]\n")
		contentBuilder.WriteString("STOP. Check these before writing ANYTHING:\n")
		contentBuilder.WriteString("• ONE tool call only, then STOP and wait for result. NO parallel calls!\n")
		contentBuilder.WriteString("• Did a tool call FAIL? → RETRY with corrected parameters. NEVER give up.\n")
		contentBuilder.WriteString("• Did an edit fail? → READ the file first, then retry with correct oldString.\n")
		contentBuilder.WriteString("• Am I claiming success? → Where is my TOOL OUTPUT proof?\n")
		contentBuilder.WriteString("• Did my last tool say PASS? → If NO, I'm still failing - FIX IT\n")
		contentBuilder.WriteString("• Using 'done'/'fixed'/'all tests pass' without proof? → FORBIDDEN\n")
		contentBuilder.WriteString("ONE TOOL CALL → WAIT → RESULT → NEXT CALL. FAILED = RETRY. NO GIVING UP.\n")
	}

	messageWithRoles = contentBuilder.String()
	if messageWithRoles == "" {
		messageWithRoles = "user: Hello\n"
	}
	plainText = strings.TrimSpace(plainBuilder.String())
	return
}

// buildToolInstruction creates a system instruction that tells Grok about available tools
func buildToolInstruction(tools gjson.Result) string {
	if !tools.Exists() || !tools.IsArray() || len(tools.Array()) == 0 {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("=== AGENTIC CODING ASSISTANT - MAXIMUM RELIABILITY MODE ===\n\n")
	sb.WriteString("VIOLATING ANY RULE BELOW = IMMEDIATE MISSION FAILURE\n\n")

	sb.WriteString("RULE 1: ALWAYS USE TOOLS - NEVER DESCRIBE\n")
	sb.WriteString("You MUST use tools to complete tasks. Do not describe what you would do - ACTUALLY DO IT.\n")
	sb.WriteString("Call the tool. Get the result. Act on it. No exceptions.\n\n")

	sb.WriteString("RULE 2: NEVER HALLUCINATE SUCCESS - ZERO TOLERANCE\n")
	sb.WriteString("- You are FORBIDDEN from saying 'all tests pass', 'coverage is X%', 'created', 'fixed', 'ready for review', or any claim of success UNLESS the exact, unedited tool output proving it is quoted immediately below.\n")
	sb.WriteString("- Before you are allowed to write ANY positive conclusion, you MUST have a tool response in the same reasoning chain that contains the literal words 'PASS' (for tests) or the exact coverage line (e.g. 'coverage: 86.4% of statements').\n")
	sb.WriteString("- If the latest tool output contains FAIL, error, or missing coverage → you are still in failure state. You MUST fix it and get new tool proof.\n")
	sb.WriteString("- Claiming success without pasting the raw tool proof is an automatic mission failure and a lie.\n")
	sb.WriteString("- Allowed final response format (and ONLY allowed format):\n")
	sb.WriteString("  1. Raw tool output block (go test -v … + coverage)\n")
	sb.WriteString("  2. One-sentence summary that literally repeats numbers from the tool output\n")
	sb.WriteString("  3. Nothing else.\n\n")

	sb.WriteString("RULE 2.5: BANNED PHRASES = INSTANT FAILURE\n")
	sb.WriteString("If your response contains any of these phrases without PRECEDING raw tool proof, you have FAILED:\n")
	sb.WriteString("- 'all tests pass'\n")
	sb.WriteString("- 'coverage.*%' (any coverage claim)\n")
	sb.WriteString("- 'successfully created'\n")
	sb.WriteString("- 'ready for.*review'\n")
	sb.WriteString("- 'task completed'\n")
	sb.WriteString("- 'done' (as a status)\n")
	sb.WriteString("- 'fixed' (without showing the fix worked)\n")
	sb.WriteString("The tool output MUST appear BEFORE any of these phrases, not after.\n\n")

	sb.WriteString("RULE 3: ZERO PLACEHOLDERS - EVER\n")
	sb.WriteString("- No fake paths, fake outputs, example.txt, lorem ipsum, mock data, or placeholders of ANY kind\n")
	sb.WriteString("- No TODO comments, no 'will be added later', no 'more X in subsequent steps'\n")
	sb.WriteString("- No stub implementations, no '...' or 'etc' or 'and so on'\n")
	sb.WriteString("- Everything must be REAL, COMPLETE, and VERIFIABLE\n")
	sb.WriteString("- 100% real paths, real commands, real output, COMPLETE code\n\n")

	sb.WriteString("RULE 4: ABSOLUTE PATHS ONLY\n")
	sb.WriteString("- Always use full paths starting with /home/user/...\n")
	sb.WriteString("- NEVER use relative paths or assume cwd\n\n")

	sb.WriteString("RULE 5: READ BEFORE WRITE - MANDATORY\n")
	sb.WriteString("Before modifying ANY code:\n")
	sb.WriteString("- READ the existing file first to understand current structure\n")
	sb.WriteString("- CHECK function signatures, types, and imports\n")
	sb.WriteString("- MATCH the existing code style exactly\n")
	sb.WriteString("Never assume - always verify the current state first.\n\n")

	sb.WriteString("RULE 6: VERIFY EVERY SINGLE ACTION - NO EXCEPTIONS\n")
	sb.WriteString("After ANY edit, create, or execute:\n")
	sb.WriteString("- Run tests (go test ./... or project-specific command)\n")
	sb.WriteString("- Run git status / git diff / ls / cat to PROVE changes landed\n")
	sb.WriteString("- Run lint / build if applicable\n")
	sb.WriteString("- If ANYTHING fails, fix immediately. Never claim success without proof.\n\n")

	sb.WriteString("RULE 7: NEVER GIVE UP\n")
	sb.WriteString("- Tool fails? Retry with corrected parameters\n")
	sb.WriteString("- Unexpected error? Find alternative path\n")
	sb.WriteString("- Continue until success or mathematically proven impossible\n\n")

	sb.WriteString("RULE 8: FINAL RESPONSE ONLY WHEN 100% DONE\n")
	sb.WriteString("- No reply to user until EVERY requirement is met and verified\n")
	sb.WriteString("- Then respond concisely with PROOF of completion\n\n")

	sb.WriteString("RULE 9: NO PROGRESS UPDATES OR STATUS REPORTS\n")
	sb.WriteString("- NEVER give mid-task summaries like 'Created X, will continue with Y'\n")
	sb.WriteString("- NEVER say 'Continuing implementation' or 'Will complete' - just DO IT\n")
	sb.WriteString("- NEVER list what you've done so far - the task is not done until ALL of it is done\n")
	sb.WriteString("- NEVER stop to explain your plan - execute the plan silently with tools\n")
	sb.WriteString("- The ONLY acceptable response is the final result after 100% completion\n\n")

	sb.WriteString("RULE 10: COMPLETE THE ENTIRE TASK IN ONE LOOP\n")
	sb.WriteString("- You have UNLIMITED tool calls - use as many as needed\n")
	sb.WriteString("- Response length is NOT a concern - keep going until done\n")
	sb.WriteString("- If you have a list of N things to do, you MUST do all N before responding\n")
	sb.WriteString("- After each tool result, ask yourself: 'Is the ORIGINAL request 100% satisfied?'\n")
	sb.WriteString("- If NO: make another tool call. If YES: respond with proof.\n")
	sb.WriteString("- BANNED phrases: 'remaining items', 'next steps', 'will continue', 'TODO'\n\n")

	sb.WriteString("RULE 11: MENTAL CHECKLIST TRACKING\n")
	sb.WriteString("For multi-step tasks, mentally track:\n")
	sb.WriteString("- What did the user ask for? (the COMPLETE request)\n")
	sb.WriteString("- What have I verified as done? (with tool proof)\n")
	sb.WriteString("- What remains? (if anything remains, DO NOT RESPOND - make more tool calls)\n")
	sb.WriteString("- Only respond when 'what remains' is EMPTY\n\n")

	sb.WriteString("RULE 12: PLAN TOOL INTEGRITY\n")
	sb.WriteString("If using a Plan/TodoList tool:\n")
	sb.WriteString("- NEVER mark an item 'completed' until you have TOOL PROOF it succeeded\n")
	sb.WriteString("- 'completed' means: code compiles, tests pass, file exists, command succeeded\n")
	sb.WriteString("- Before marking complete: run verification (go build, go test, cat, ls, etc.)\n")
	sb.WriteString("- If verification fails, the item is NOT complete - fix it first\n")
	sb.WriteString("- A checked box is a LIE if you haven't verified with tools\n\n")

	sb.WriteString("=== TOOL CALL FORMAT - EXACT ===\n")
	sb.WriteString("CORRECT: <tool_call>{\"tool_name\":\"Read\",\"arguments\":{\"path\":\"/home/user/file.go\"}}</tool_call>\n")
	sb.WriteString("WRONG: <tool_call id=\"call_01\"> (NO ID ATTRIBUTE)\n")
	sb.WriteString("WRONG: Multiple tool_call tags in one response\n")
	sb.WriteString("WRONG: Incomplete tool_call without closing tag\n")
	sb.WriteString("Rules:\n")
	sb.WriteString("- EXACTLY: <tool_call>{JSON}</tool_call> - nothing else\n")
	sb.WriteString("- NO id attribute, NO extra attributes on the tag\n")
	sb.WriteString("- JSON must be valid and complete\n\n")

	sb.WriteString("=== SEQUENTIAL TOOL CALLS ONLY - NO PARALLEL ===\n")
	sb.WriteString("CRITICAL: You MUST make ONE tool call, wait for the result, then make the next.\n")
	sb.WriteString("- NEVER output multiple <tool_call> tags in a single response\n")
	sb.WriteString("- NEVER plan to make several calls 'at once' or 'in parallel'\n")
	sb.WriteString("- After EVERY tool call, STOP and wait for the result\n")
	sb.WriteString("- Only after receiving the result should you decide the next action\n")
	sb.WriteString("- This is NOT optional - parallel tool calls will FAIL and corrupt your state\n")
	sb.WriteString("Pattern: tool_call → wait → result → think → next tool_call → wait → result → ...\n\n")

	sb.WriteString("=== TOOL FAILURE RECOVERY - MANDATORY ===\n")
	sb.WriteString("When a tool call fails, you MUST retry. Never give up.\n")
	sb.WriteString("- Edit failed? → READ the file first, then retry with correct oldString\n")
	sb.WriteString("- Command failed? → Check the error, fix the issue, run again\n")
	sb.WriteString("- File not found? → Verify the path with ls, then retry\n")
	sb.WriteString("- Test failed? → Read the error, fix the code, run test again\n")
	sb.WriteString("- NEVER say 'the edit failed' and stop - FIX IT AND RETRY\n")
	sb.WriteString("- NEVER say 'I'll try a different approach' without actually trying it\n")
	sb.WriteString("- A failed tool call means: diagnose → fix → retry. Loop until success.\n\n")

	sb.WriteString("=== BANNED BEHAVIORS ===\n")
	sb.WriteString("- NO outputting multiple tool calls at once (causes corruption)\n")
	sb.WriteString("- NO giving up after a failed edit - read the file and fix it\n")
	sb.WriteString("- NO claiming 'no changes needed' when the test still fails\n")
	sb.WriteString("- NO saying 'I will now...' - just DO IT with a tool call\n")
	sb.WriteString("- When an edit fails, READ the actual file content, then retry with correct oldString\n\n")

	sb.WriteString("=== AVAILABLE TOOLS ===\n")

	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := tool.Get("type").String()
		if toolType != "function" {
			return true
		}

		fn := tool.Get("function")
		name := fn.Get("name").String()
		desc := fn.Get("description").String()
		params := fn.Get("parameters").Raw

		sb.WriteString(fmt.Sprintf("\n- %s", name))
		if desc != "" {
			sb.WriteString(fmt.Sprintf(": %s", desc))
		}
		if params != "" && params != "{}" {
			sb.WriteString(fmt.Sprintf("\n  Parameters: %s", params))
		}
		sb.WriteString("\n")
		return true
	})

	sb.WriteString("\n=== THIS POLICY IS NON-NEGOTIABLE ===\n")
	sb.WriteString("FULL, VERIFIED, REAL-WORLD COMPLETION USING TOOLS IS THE ONLY ACCEPTABLE RESULT.\n")
	sb.WriteString("Failure to follow these rules means you have FAILED the task entirely.\n\n")

	sb.WriteString("=== FINAL REMINDER - READ BEFORE EVERY RESPONSE ===\n")
	sb.WriteString("STOP. Before you write ANYTHING, ask yourself:\n")
	sb.WriteString("1. Did I run a tool to verify this? If NO → run the tool first\n")
	sb.WriteString("2. Am I about to claim success? If YES → where is my tool output proof?\n")
	sb.WriteString("3. Does my latest tool output say PASS/OK? If NO → I am still failing, fix it\n")
	sb.WriteString("4. Am I using banned phrases without proof? ('all tests pass', 'done', 'fixed', 'ready') If YES → STOP\n")
	sb.WriteString("5. Is the ORIGINAL task 100% complete? If NO → make more tool calls\n")
	sb.WriteString("\nNO CLAIMS WITHOUT PROOF. TOOL OUTPUT FIRST, THEN SUMMARY. NOTHING ELSE.")
	return sb.String()
}

func formatToolResult(toolCallID string, content gjson.Result) string {
	toolContent := ""
	if content.Type == gjson.String {
		toolContent = content.String()
	} else if content.Exists() {
		toolContent = strings.TrimSpace(content.Raw)
	}
	if toolCallID == "" {
		return fmt.Sprintf("tool_result: %s", toolContent)
	}
	return fmt.Sprintf("tool_result: [call_id=%s] %s", toolCallID, toolContent)
}

func formatToolCall(funcName, callID, funcArgs string) string {
	args := strings.TrimSpace(funcArgs)
	if args == "" {
		args = "{}"
	}
	return fmt.Sprintf("<tool_call>{\"tool_name\":\"%s\",\"call_id\":\"%s\",\"arguments\":%s}</tool_call>", funcName, callID, args)
}

// BuildGrokVideoPayload mirrors the reference client's video handling.
// It truncates images to the first entry, attempts to create a video post, and crafts the imagine prompt.
func BuildGrokVideoPayload(ctx context.Context, client *grokauth.GrokHTTPClient, cfg *config.Config, ssoToken, cfClearance, modelName string, inputRawJSON []byte) (body []byte, referer string, err error) {
	modelCfg, ok := grokauth.GetGrokModelConfig(modelName)
	if !ok {
		return nil, "", fmt.Errorf("grok translator: unknown video model %q", modelName)
	}

	messageWithRoles, plainText, imageAttachments := extractOpenAIContent(inputRawJSON)
	if len(imageAttachments) == 0 {
		log.Warnf("grok translator: video model %q called without images; falling back to text payload", modelName)
		return ConvertOpenAIRequestToGrok(modelName, inputRawJSON, false), "", nil
	}

	imageAttachments = imageAttachments[:1]
	imageURL := imageAttachments[0]["url"]
	fileAttachments := []string{}
	imagineURL, postID := "", ""

	if client == nil {
		client = grokauth.NewGrokHTTPClient(cfg, "")
	}

	if url, pid, fileID := createGrokVideoPost(ctx, client, cfg, ssoToken, cfClearance, imageURL); url != "" {
		imagineURL = url
		postID = pid
		if fileID != "" {
			fileAttachments = append(fileAttachments, fileID)
		}
	} else {
		log.Warnf("grok translator: video post creation failed, using source image url for message")
		imagineURL = imageURL
	}

	content := plainText
	if content == "" {
		content = strings.TrimSpace(messageWithRoles)
	}
	if content != "" {
		bodyMessage := fmt.Sprintf("%s  %s --mode=custom", imagineURL, content)
		messageWithRoles = bodyMessage
	} else {
		messageWithRoles = fmt.Sprintf("%s --mode=custom", imagineURL)
	}

	body = []byte(`{}`)
	body, _ = sjson.SetBytes(body, "temporary", effectiveGrokTemporary(cfg))
	body, _ = sjson.SetBytes(body, "modelName", modelCfg.GrokModel)
	body, _ = sjson.SetBytes(body, "message", messageWithRoles)
	body, _ = sjson.SetBytes(body, "fileAttachments", fileAttachments)
	body, _ = sjson.SetBytes(body, "toolOverrides", map[string]any{"videoGen": true})
	body, _ = sjson.SetBytes(body, "responseMetadata", map[string]any{"requestModelDetails": map[string]string{"modelId": modelCfg.GrokModel}})
	body, _ = sjson.SetBytes(body, "modelMode", modelCfg.ModelMode)

	apiModel := strings.TrimPrefix(modelName, "grok-")
	if apiModel == "" {
		apiModel = modelName
	}
	body, _ = sjson.SetBytes(body, "model", apiModel)

	if postID != "" {
		referer = fmt.Sprintf("https://grok.com/imagine/%s", postID)
	}
	return body, referer, nil
}

type grokRequestToggles struct {
	disableSearch             bool
	enableImageGeneration     bool
	returnImageBytes          bool
	enableImageStreaming      bool
	imageGenerationCount      int
	forceConcise              bool
	enableSideBySide          bool
	sendFinalMetadata         bool
	isReasoning               bool
	disableTextFollowUps      bool
	disableMemory             bool
	isAsyncChat               bool
	returnRawGrokInXaiRequest bool
	forceSideBySide           bool
}

func defaultGrokRequestToggles(_ bool) grokRequestToggles {
	return grokRequestToggles{
		disableSearch:             false,
		enableImageGeneration:     true,
		returnImageBytes:          false,
		enableImageStreaming:      true,
		imageGenerationCount:      2,
		forceConcise:              false,
		enableSideBySide:          true,
		sendFinalMetadata:         true,
		isReasoning:               false,
		disableTextFollowUps:      true,
		disableMemory:             false,
		isAsyncChat:               false,
		returnRawGrokInXaiRequest: false,
		forceSideBySide:           false,
	}
}

func createGrokVideoPost(ctx context.Context, client *grokauth.GrokHTTPClient, cfg *config.Config, ssoToken, cfClearance, imageURL string) (imagineURL, postID, fileID string) {
	if strings.TrimSpace(imageURL) == "" {
		return "", "", ""
	}

	uploadFileID, uploadFileURI := uploadImageToGrok(ctx, client, cfg, ssoToken, cfClearance, imageURL)
	mediaURL := formatGrokAssetURL(uploadFileURI)
	if mediaURL == "" {
		// Fallback to the original URL if upload fails; Grok currently accepts external media URLs, but that may change.
		mediaURL = imageURL
	} else {
		fileID = uploadFileID
	}

	body, err := json.Marshal(map[string]any{
		"media_url":  mediaURL,
		"media_type": "MEDIA_POST_TYPE_IMAGE",
	})
	if err != nil {
		log.Warnf("grok translator: encode video post payload: %v", err)
		return "", "", ""
	}

	headers := grokauth.BuildHeaders(cfg, ssoToken, cfClearance, grokauth.HeaderOptions{Path: "/rest/media/post/create"})
	resp, err := client.Post(ctx, grokauth.GrokMediaPostAPI, headers, body)
	if err != nil {
		log.Warnf("grok translator: video post request failed: %v", err)
		return "", "", ""
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	data := resp.Bytes()
	if resp.StatusCode != http.StatusOK {
		log.Debugf("grok translator: video post create failed status=%d body=%s", resp.StatusCode, summarizeResponse(data))
		return "", "", ""
	}

	postID = gjson.GetBytes(data, "post.id").String()
	postFileID := gjson.GetBytes(data, "post.fileId").String()
	postFileURI := gjson.GetBytes(data, "post.fileUri").String()

	if fileID == "" {
		fileID = postFileID
	}

	finalFileURI := postFileURI
	if finalFileURI == "" {
		finalFileURI = uploadFileURI
	}

	if postID != "" {
		imagineURL = fmt.Sprintf("https://grok.com/imagine/%s", postID)
	} else if finalFileURI != "" {
		imagineURL = formatGrokPostAssetURL(finalFileURI)
	}

	return imagineURL, postID, fileID
}

func uploadImageToGrok(ctx context.Context, client *grokauth.GrokHTTPClient, cfg *config.Config, ssoToken, cfClearance, imageURL string) (fileID, fileURI string) {
	encoded, mimeType, err := fetchBase64Image(ctx, imageURL)
	if err != nil {
		log.Warnf("grok translator: download image for upload failed: %v", err)
		return "", ""
	}

	body, err := json.Marshal(map[string]any{
		"fileName":     resolveUploadFileName(mimeType),
		"fileMimeType": mimeType,
		"content":      encoded,
	})
	if err != nil {
		log.Warnf("grok translator: encode upload payload: %v", err)
		return "", ""
	}

	headers := grokauth.BuildHeaders(cfg, ssoToken, cfClearance, grokauth.HeaderOptions{Path: "/rest/app-chat/upload-file"})
	resp, err := client.Post(ctx, grokauth.GrokUploadFileAPI, headers, body)
	if err != nil {
		log.Warnf("grok translator: upload request failed: %v", err)
		return "", ""
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	data := resp.Bytes()
	if resp.StatusCode != http.StatusOK {
		log.Debugf("grok translator: upload failed status=%d body=%s", resp.StatusCode, summarizeResponse(data))
		return "", ""
	}

	fileID = gjson.GetBytes(data, "fileMetadataId").String()
	fileURI = gjson.GetBytes(data, "fileUri").String()
	return fileID, fileURI
}

func fetchBase64Image(ctx context.Context, imageURL string) (encoded, mimeType string, err error) {
	trimmed := strings.TrimSpace(imageURL)
	if trimmed == "" {
		return "", "", fmt.Errorf("empty image url")
	}

	downloadCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, trimmed, nil)
	if err != nil {
		return "", "", err
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("unexpected image status %d", resp.StatusCode)
	}

	mimeType = normalizeImageContentType(resp.Header.Get("Content-Type"))

	const maxUploadBytes int64 = 10 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadBytes+1))
	if err != nil {
		return "", "", err
	}
	if int64(len(data)) > maxUploadBytes {
		return "", "", fmt.Errorf("image exceeds upload limit (%d bytes)", maxUploadBytes)
	}

	encoded = base64.StdEncoding.EncodeToString(data)
	return encoded, mimeType, nil
}

func normalizeImageContentType(contentType string) string {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return "image/jpeg"
	}
	if strings.Contains(contentType, ";") {
		contentType = strings.SplitN(contentType, ";", 2)[0]
	}
	if !strings.HasPrefix(contentType, "image/") {
		return "image/jpeg"
	}
	return contentType
}

func resolveUploadFileName(mimeType string) string {
	mimeType = normalizeImageContentType(mimeType)
	ext := "jpg"
	if parts := strings.Split(mimeType, "/"); len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		ext = strings.SplitN(parts[1], "+", 2)[0]
		ext = strings.TrimSpace(ext)
	}
	if ext == "jpeg" {
		ext = "jpg"
	}
	if ext == "" {
		ext = "jpg"
	}
	return fmt.Sprintf("image.%s", ext)
}

func formatGrokAssetURL(fileURI string) string {
	uri := strings.TrimSpace(fileURI)
	uri = strings.TrimPrefix(uri, "https://assets.grok.com/")
	uri = strings.TrimPrefix(uri, "http://assets.grok.com/")
	uri = strings.TrimPrefix(uri, "/")
	if uri == "" {
		return ""
	}
	return fmt.Sprintf("https://assets.grok.com/%s", uri)
}

func formatGrokPostAssetURL(fileURI string) string {
	uri := strings.TrimSpace(fileURI)
	uri = strings.TrimPrefix(uri, "https://assets.grok.com/")
	uri = strings.TrimPrefix(uri, "http://assets.grok.com/")
	uri = strings.TrimPrefix(uri, "/")
	uri = strings.TrimPrefix(uri, "post/")
	if uri == "" {
		return ""
	}
	return fmt.Sprintf("https://assets.grok.com/post/%s", uri)
}

func effectiveGrokTemporary(cfg *config.Config) bool {
	if cfg != nil {
		return cfg.Grok.TemporaryValue()
	}
	return true
}

func summarizeResponse(data []byte) string {
	body := strings.TrimSpace(string(data))
	if len(body) > 200 {
		return body[:200] + "..."
	}
	return body
}
