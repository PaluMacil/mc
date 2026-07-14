// Package views holds the templ components and their view models for mc-invite.
// Templates depend only on these plain-string view models, never on the domain
// or storage types, so they stay trivially testable and prefix-agnostic (all
// URLs arrive pre-built from the handler).
package views

// NavVM is the shared header. All URLs are absolute paths already carrying the
// app's base path.
type NavVM struct {
	HomeURL   string
	LoginURL  string
	LogoutURL string
	HtmxSrc   string
	SignedIn  bool
	Email     string
	IsAdmin   bool
	IsInviter bool
	HideAuth  bool // public pages (invite redemption) show no auth controls
}

// InviteRowVM is one row of the invite table.
type InviteRowVM struct {
	CreatedAt     string
	ExpiresAt     string
	Status        string // active, used, or expired
	MinecraftName string
	CreatedBy     string // shown only in the admin (all-inviters) view
}

// HomeVM drives the inviter/admin dashboard.
type HomeVM struct {
	Nav       NavVM
	CanMint   bool
	MintURL   string
	AdminView bool   // the signed-in user is an admin (can see the toggle and audit)
	ShowAll   bool   // currently showing every inviter's invites
	ToggleURL string // link that flips between "mine" and "all"
	ShowOwner bool   // render the "created by" column
	Invites   []InviteRowVM
	Audit     []AuditRowVM
}

// AuditRowVM is one row of the admin audit table.
type AuditRowVM struct {
	At     string
	Actor  string
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
	State     string // form, used, expired, or invalid
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
