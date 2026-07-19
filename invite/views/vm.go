// Package views holds the templ components and their view models for mc-invite.
// Templates depend only on these plain-string view models, never on the domain
// or storage types, so they stay trivially testable and prefix-agnostic (all
// URLs arrive pre-built from the handler).
package views

// NavVM is the shared header. All URLs are absolute paths already carrying the
// app's base path (or the site-relative landing/map paths).
type NavVM struct {
	HomeURL      string // portal dashboard (the "Dashboard" member link)
	DownloadsURL string // client pack + setup guide (member link)
	LandingURL   string // main site landing page (the brand + "Home")
	MapURL       string // live world map
	TipsURL       string // ATM10 tips page (on the landing site)
	ParentsURL    string // parent tips page (on the landing site)
	RulesURL      string // house rules page (on the landing site)
	CurseforgeURL string // CurseForge install guide (on the landing site)
	VanillaURL    string // vanilla-launcher install guide (on the landing site)
	MetricsURL    string // Grafana dashboard, shown in the admin dropdown only
	LoginURL     string
	LogoutURL    string
	HtmxSrc      string
	SignedIn     bool
	Name         string
	IsAdmin      bool
	IsInviter    bool
	HideAuth     bool // public pages (invite redemption) show no auth controls
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

// DownloadsVM drives the authenticated downloads page: the whole mc-mods bucket,
// split into the client pack (what a vanilla-launcher player wants) and the
// server files/mods (listed for completeness, not needed to play).
type DownloadsVM struct {
	Nav           NavVM
	Available     bool             // false when R2 is unavailable; hides the links
	Client        []DownloadFileVM // the client pack(s) most people want
	Server        []DownloadFileVM // server files & mods; not needed to play
	CurseforgeURL string           // link to the CurseForge install guide
	VanillaURL    string           // link to the vanilla-launcher install guide
}

// DownloadFileVM is one downloadable object. URL is a GET endpoint on this app
// that 302-redirects to a short-lived presigned R2 URL.
type DownloadFileVM struct {
	Title string
	Desc  string
	Size  string // human-readable, e.g. "1.5 GB"
	URL   string
}
