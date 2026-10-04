package cmd

import "github.com/router-for-me/CLIProxyAPI/v8/internal/config"

// DoOpenCodeLogin signs in to an OpenCode Console workspace with the device
// flow and saves the Go subscription credential.
func DoOpenCodeLogin(cfg *config.Config, options *LoginOptions) {
	doKimiLoginWithProvider(cfg, options, "opencode-go", "OpenCode Go")
}
