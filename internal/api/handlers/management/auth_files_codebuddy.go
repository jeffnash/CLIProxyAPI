package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// codeBuddyManagementHTTPClient builds the management login HTTP client:
// proxy-aware with zero timeout. Credential calls apply their own
// per-request deadline. Tests override this factory.
var codeBuddyManagementHTTPClient = func(cfg *config.Config) *http.Client {
	client := &http.Client{}
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if strings.TrimSpace(sdkCfg.ProxyURL) == "" {
			sdkCfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	return util.SetProxy(&sdkCfg, client)
}

// codeBuddyManagementPollInterval is the delay between authorization polls.
var codeBuddyManagementPollInterval = 2 * time.Second

// RequestCodeBuddyToken starts a CodeBuddy browser authorization session for
// the explicit ?realm= profile and polls it in the background. Cancellation
// during token, account or catalog phases prevents persistence.
func (h *Handler) RequestCodeBuddyToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	realmName := strings.ToLower(strings.TrimSpace(c.Query("realm")))
	if realmName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing realm (want cn, global or workbuddy-global)"})
		return
	}
	realm, err := codebuddy.ParseRealm(realmName, "")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	fmt.Println("Initializing CodeBuddy authentication...")

	client := codebuddy.NewClient(codeBuddyManagementHTTPClient(h.cfg))
	session, err := client.StartLogin(ctx, realm)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	localState := "cb-" + codeBuddyRandomSessionID()
	RegisterOAuthSession(localState, codebuddy.Provider)

	go func() {
		pollCtx, cancelPoll := context.WithTimeout(ctx, codebuddy.LoginLifetime)
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, localState, codebuddy.Provider)

		fmt.Println("Waiting for authentication...")
		credentials, err := pollCodeBuddyManagementLogin(pollCtx, client, session, localState)
		if err != nil {
			if !IsOAuthSessionPending(localState, codebuddy.Provider) {
				return
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				SetOAuthSessionError(localState, "login session expired")
				fmt.Println("CodeBuddy authentication expired")
				return
			}
			SetOAuthSessionError(localState, oauthSessionErrorWithCause("Authentication failed", err))
			fmt.Printf("Authentication failed: %v\n", err)
			return
		}
		if !IsOAuthSessionPending(localState, codebuddy.Provider) {
			return
		}
		// Catalog discovery runs under the same login session context, so it
		// cannot outlive the 15-minute auth lifetime.
		catalog, err := client.FetchCatalog(pollCtx, credentials)
		if err != nil {
			if !IsOAuthSessionPending(localState, codebuddy.Provider) {
				return
			}
			SetOAuthSessionError(localState, "CodeBuddy model discovery failed")
			fmt.Printf("CodeBuddy model discovery failed: %v\n", err)
			return
		}
		if !IsOAuthSessionPending(localState, codebuddy.Provider) {
			return
		}
		record := sdkAuth.NewCodeBuddyAuthRecord(credentials, catalog, time.Now())
		if errGuard := guardOAuthSessionPendingForSave(localState, codebuddy.Provider); errGuard != nil {
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(localState, "Failed to save authentication tokens")
			return
		}
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use CodeBuddy services through this CLI")
		CompleteOAuthSession(localState)
	}()

	c.JSON(200, gin.H{
		"status":     "ok",
		"url":        session.AuthURL,
		"state":      localState,
		"flow":       "device",
		"expires_in": int(codebuddy.LoginLifetime / time.Second),
		"realm":      string(realm),
	})
}

// pollCodeBuddyManagementLogin polls a login session until the operator
// authorizes, the context ends, or the session is cancelled. The interval is
// captured once per worker: rereading the mutable global on every poll races
// test cleanup that restores it after the worker was observed to stop.
func pollCodeBuddyManagementLogin(ctx context.Context, client *codebuddy.Client, session codebuddy.LoginSession, localState string) (codebuddy.Credentials, error) {
	interval := codeBuddyManagementPollInterval
	for {
		if !IsOAuthSessionPending(localState, codebuddy.Provider) {
			return codebuddy.Credentials{}, context.Canceled
		}
		credentials, ready, err := client.PollLogin(ctx, session)
		if err != nil {
			return codebuddy.Credentials{}, err
		}
		if ready {
			return credentials, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return codebuddy.Credentials{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// codeBuddyRandomSessionID generates a random local session suffix that is
// independent from the upstream authorization state.
func codeBuddyRandomSessionID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}
