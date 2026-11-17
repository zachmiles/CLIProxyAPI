package modelinventory

// Limits holds token limits; 0 means unknown (omitted on the wire).
type Limits struct{ Context, Input, Output int64 }

// Candidate is one model an eligible credential would serve BEFORE exclusions are applied.
// One Candidate per (credential, served ID). The source never filters by rules.
type Candidate struct {
	Channel string // rules key: codex, opencode-go, claude, xai, zai, ...
	ID      string // served ID a client sends (alias and prefix applied)
	RuleID  string // ID the exclusion matcher evaluates (upstream ID); "" means same as ID
	BaseID  string // base served ID for routed variants (e.g. "-fast"), "" otherwise
	// RuleIDs, when present, lists the exact IDs checked for this credential's
	// route. RuleID and BaseID still describe the route on the wire.
	RuleIDs       []string
	Published     bool // route survived registration for this credential
	Name          string
	Kind          string // "" -> reported as "chat"
	Family        string
	Limits        Limits
	Efforts       []string
	CredentialKey string // opaque, stable per credential; NEVER serialized
	// CredentialRules is non-nil when this credential's own excluded list replaces the channel rules.
	CredentialRules []string
}

// Snapshot ties pre-exclusion candidates and their published status to the
// configuration used to build them.
type Snapshot struct {
	Candidates []Candidate
	Rules      map[string][]string
	SourceHash string
}

// Source returns the published model inventory. Must be safe for concurrent use.
type Source interface{ InventorySnapshot() Snapshot }
