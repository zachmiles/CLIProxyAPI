package opencode

import (
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// SessionHeader is the conversation ID Go uses for routing and prompt caching.
const SessionHeader = "X-Opencode-Session"

// ApplySessionHeader gives Go the client's own session ID on Console requests.
// The credential's $CPA-SESSION-ID header expands to the router's affinity key,
// which carries a source prefix ("header:ses_…"); OpenCode's own header, which
// names the parent session for subagents, is preferred over it.
func ApplySessionHeader(req *http.Request, auth *cliproxyauth.Auth, clientHeaders http.Header) {
	if req == nil || !IsConsoleAuth(auth) {
		return
	}
	if id := strings.TrimSpace(clientHeaders.Get(SessionHeader)); id != "" {
		req.Header.Set(SessionHeader, id)
		return
	}
	id := strings.TrimSpace(req.Header.Get(SessionHeader))
	for stripped := true; stripped; {
		stripped = false
		for _, prefix := range session.CandidateSessionPrefixes {
			if rest, ok := strings.CutPrefix(id, prefix); ok && rest != "" {
				id, stripped = rest, true
			}
		}
	}
	if id == "" {
		req.Header.Del(SessionHeader)
		return
	}
	req.Header.Set(SessionHeader, id)
}
