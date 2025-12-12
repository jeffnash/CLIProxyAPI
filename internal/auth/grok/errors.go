package grok

import "errors"

var (
	ErrNoSSOToken        = errors.New("no SSO token available")
	ErrTokenExpired      = errors.New("grok token has expired")
	ErrRateLimited       = errors.New("grok rate limit exceeded")
	ErrCloudflareBlocked = errors.New("blocked by Cloudflare - update cf_clearance or use proxy")
	ErrAuthFailed        = errors.New("grok authentication failed")
)
