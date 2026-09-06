package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// passthruUpstreamModel returns the model name to send upstream, honoring a
// passthru route's upstream_model attribute override and falling back to
// baseModel. This keeps the non-streaming Execute/CountTokens paths consistent
// with ExecuteStream so passthru routes that rename the upstream model (e.g. a
// local alias mapped to a different provider model) forward the correct name.
func passthruUpstreamModel(auth *cliproxyauth.Auth, baseModel string) string {
	modelForUpstream := baseModel
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["upstream_model"]); v != "" {
			modelForUpstream = thinking.ParseSuffix(v).ModelName
		}
	}
	if strings.TrimSpace(modelForUpstream) == "" {
		modelForUpstream = baseModel
	}
	return modelForUpstream
}
