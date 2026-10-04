package auth

import (
	"context"
	"fmt"
	"time"

	opencodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// OpenCodeAuthenticator signs in to an OpenCode Console workspace for its Go
// subscription with the device-code flow.
type OpenCodeAuthenticator struct{}

// NewOpenCodeAuthenticator constructs an OpenCode Console authenticator.
func NewOpenCodeAuthenticator() Authenticator { return &OpenCodeAuthenticator{} }

// Provider returns the openai-compatibility group that serves the Go models.
func (OpenCodeAuthenticator) Provider() string { return opencodeauth.Provider }

// RefreshLead refreshes ten minutes before the access token expires.
func (OpenCodeAuthenticator) RefreshLead() *time.Duration {
	lead := 10 * time.Minute
	return &lead
}

// Login prints a verification URL and code, then waits for approval.
func (a OpenCodeAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client := opencodeauth.NewClient(cfg)
	code, err := client.StartDeviceFlow(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("\nOpen %s on any device, choose the workspace with your Go subscription, and confirm the code %s.\n\n", code.VerificationURI, code.UserCode)
	if (opts == nil || !opts.NoBrowser) && browser.IsAvailable() {
		_ = browser.OpenURL(code.VerificationURI)
	}
	fmt.Println("Waiting for approval...")
	tokens, err := client.PollForTokens(ctx, code)
	if err != nil {
		return nil, err
	}
	if tokens.OrgID == "" {
		return nil, fmt.Errorf("opencode console: the login is not bound to a workspace")
	}
	fileName := fmt.Sprintf("%s-%s.json", opencodeauth.Provider, tokens.OrgID)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    "OpenCode Go " + tokens.OrgID,
		Metadata: opencodeauth.ApplyTokens(nil, tokens, time.Now()),
	}, nil
}
