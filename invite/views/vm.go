// Package views holds the templ components and their view models for mc-invite.
// Templates depend only on these plain-string view models, never on the domain
// or storage types, so they stay trivially testable and prefix-agnostic (all
// URLs arrive pre-built from the handler).
package views

// NavVM is the shared header. All URLs are absolute paths already carrying the
// app's base path (or the site-relative landing/map paths).
type NavVM struct {
	HomeURL    string // portal dashboard
	LandingURL string // main site landing page
	MapURL     string // live world map
	LoginURL   string
	LogoutURL  string
	HtmxSrc    string
	SignedIn   bool
	Name       string
	IsAdmin    bool
	IsInviter  bool
	HideAuth   bool // public pages (invite redemption) show no auth controls
}

// InviteRowVM is one row of the invite table.
type InviteRowVM struct {
	CreatedAt     string
	ExpiresAt     string
	Status        string // active, used, canceled, or expired
	MinecraftName string
	CreatedBy     string // display name, shown only in the admin (all) view
	ShowOwner     bool   // render the "created by" cell
	CanCancel     bool
	CancelURL     string // htmx POST target when CanCancel
}

// PlayersVM drives the online-players widget (used on the portal and, via the
// same public fragment, on the landing page).
type PlayersVM struct {
	Available bool // false when RCON could not be reached
	Online    int
	Max       int
	Names     []string // online player names, rendered as a list of chips
	MapURL    string
}

// HomeVM drives the inviter/admin dashboard.
type HomeVM struct {
	Nav        NavVM
	PlayersURL string // fragment endpoint the status strip polls
	CanMint    bool
	MintURL    string
	AdminView  bool   // the signed-in user is an admin (can see the toggle and audit)
	ShowAll    bool   // currently showing every inviter's invites
	ToggleURL  string // link that flips between "mine" and "all"
	ShowOwner  bool   // render the "created by" column
	Invites    []InviteRowVM
	Audit      []AuditRowVM
}

// AuditRowVM is one row of the admin audit table.
type AuditRowVM struct {
	At     string
	Who    string
	Action string
	Detail string
}

// MintedVM is the fragment returned after minting an invite; the raw link is
// shown exactly once because only its hash is stored.
type MintedVM struct {
	Link      string
	ExpiresAt string
}

// RedeemVM drives the public redemption page before submission.
type RedeemVM struct {
	Nav       NavVM
	State     string // form, used, canceled, expired, or invalid
	SubmitURL string
	Username  string
	Error     string
}

// RedeemDoneVM drives the success page after a redemption. Failures re-render
// the Redeem form with an inline error instead of a separate page.
type RedeemDoneVM struct {
	Nav           NavVM
	MinecraftName string
	ServerAddress string
}
