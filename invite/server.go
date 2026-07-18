package main

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PaluMacil/mc/invite/views"
	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"
)

//go:embed assets/htmx.min.js
var htmxJS []byte

// minecraftRCON is the server's view of RCON: grant whitelist access and read
// who is online. It is an interface at the consumer so tests can stub it.
type minecraftRCON interface {
	WhitelistAdd(ctx context.Context, name string) (string, error)
	ListPlayers(ctx context.Context) (OnlinePlayers, error)
}

// Server holds the wired dependencies and serves the app.
type Server struct {
	cfg      Config
	store    *Store
	auth     *Auth
	sessions *scs.SessionManager
	mojang   MojangResolver
	rcon     minecraftRCON
	presign  presigner // nil when R2 is not configured; Downloads shows unavailable
	players  *playersCache
	limiter  *ipLimiter
	loc      *time.Location
	log      *slog.Logger
}

// Handler builds the routed, session-wrapped, CSRF-protected handler. Routes for
// the app live under the configured base path; the health probes sit at the
// root so Kubernetes reaches them without the ingress prefix.
func (s *Server) Handler() http.Handler {
	base := s.cfg.BasePath
	mux := http.NewServeMux()

	// Health/readiness at the true root, bypassing the app's base path.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	// Vendored htmx, self-hosted (no external origins).
	mux.HandleFunc("GET "+base+"/assets/htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(htmxJS)
	})

	// Public: who is online. Not behind login, so the landing page can embed it.
	mux.HandleFunc("GET "+base+"/players", s.playersHandler)

	// Public: session probe. The static landing page (a separate binary with no
	// access to this app's session) calls it to show the signed-in name instead
	// of a perpetual "Sign in" link.
	mux.HandleFunc("GET "+base+"/whoami", s.whoami)

	// Inviter/admin dashboard.
	mux.HandleFunc("GET "+base+"/{$}", s.auth.requireAuth(s.home))
	// Client downloads + setup guide, for any signed-in user (guests included,
	// so an invited friend can grab the pack before they are whitelisted).
	mux.HandleFunc("GET "+base+"/downloads", s.auth.requireAuth(s.downloads))
	mux.HandleFunc("GET "+base+"/downloads/{id}", s.auth.requireAuth(s.download))
	mux.HandleFunc("GET "+base+"/login", s.auth.Login)
	mux.HandleFunc("GET "+base+"/auth/callback", s.auth.Callback)
	mux.HandleFunc("POST "+base+"/logout", s.auth.Logout)
	mux.HandleFunc("POST "+base+"/invites", s.auth.requireRole("inviter", s.mint))
	mux.HandleFunc("POST "+base+"/invites/{id}/cancel", s.auth.requireRole("inviter", s.cancelInvite))

	// Public redemption.
	mux.HandleFunc("GET "+base+"/i/{token}", s.redeemForm)
	mux.HandleFunc("POST "+base+"/i/{token}", s.redeem)

	// Bare base path (no trailing slash) redirects into the app.
	if base != "" {
		mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, base+"/", http.StatusFound)
		})
	}

	// Sessions wrap everything; CSRF (Sec-Fetch-Site based) guards unsafe
	// methods. Same-origin form posts (mint, cancel, logout, redeem) pass; the
	// only cross-site entry, the OIDC callback, is a GET and is not checked.
	csrf := http.NewCrossOriginProtection()
	return csrf.Handler(s.sessions.LoadAndSave(mux))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)

	// Signed in but not yet granted a role: show the pending page, not the
	// dashboard. An admin promotes them by adding them to a group in Authentik.
	if !u.IsInviter() && !u.IsAdmin() {
		s.render(w, r, views.Pending(s.nav(u, false)))
		return
	}

	showAll := u.IsAdmin() && r.URL.Query().Get("all") == "1"
	owner := ""
	if !showAll {
		owner = u.Subject
	}
	invites, err := s.store.ListInvites(ctx, owner, 100)
	if err != nil {
		s.serverError(w, "listing invites", err)
		return
	}

	vm := views.HomeVM{
		Nav:        s.nav(u, false),
		PlayersURL: s.cfg.BasePath + "/players",
		CanMint:    u.IsInviter(),
		MintURL:    s.cfg.BasePath + "/invites",
		AdminView:  u.IsAdmin(),
		ShowAll:    showAll,
		ShowOwner:  showAll,
		Invites:    s.inviteRows(invites, u, showAll),
	}
	if showAll {
		vm.ToggleURL = s.cfg.BasePath + "/"
	} else {
		vm.ToggleURL = s.cfg.BasePath + "/?all=1"
	}

	if u.IsAdmin() {
		if audit, err := s.store.RecentAudit(ctx, 50); err == nil {
			vm.Audit = s.auditRows(audit)
		} else {
			s.log.Warn("loading audit log", "err", err)
		}
	}

	s.render(w, r, views.Home(vm))
}

// Modpack coordinates shown on the downloads page; keep these in step with the
// running server (NEOFORGE_VERSION and the pack version in the deploy manifest).
const (
	atmPackVersion  = "7.1"
	neoForgeVersion = "21.1.234"
)

// downloadItem is one file offered on the downloads page. The id is the URL
// segment; the R2 object key is resolved server-side from this fixed table so a
// signed-in user can never presign an arbitrary bucket object.
type downloadItem struct {
	id     string
	object string
	title  string
	desc   string
}

func (s *Server) downloadItems() []downloadItem {
	return []downloadItem{{
		id:     "client",
		object: s.cfg.ClientPackObject,
		title:  "ATM10 client pack (" + atmPackVersion + ")",
		desc: "One zip with everything the game needs: all mods, the pack configs, " +
			"and KubeJS scripts. Extract it into your game folder after installing " +
			"NeoForge (steps below).",
	}}
}

func (s *Server) findDownload(id string) (downloadItem, bool) {
	for _, it := range s.downloadItems() {
		if it.id == id {
			return it, true
		}
	}
	return downloadItem{}, false
}

// downloads renders the client-pack links and the vanilla-launcher setup guide.
// Any signed-in user may view it (guests included).
func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	vm := views.DownloadsVM{
		Nav:           s.nav(u, false),
		Available:     s.presign != nil,
		ServerAddress: s.cfg.ServerAddress,
		FallbackAddr:  "game.danwolf.net:25999",
		MapURL:        s.cfg.MapURL,
		NeoForge:      neoForgeVersion,
		PackVersion:   atmPackVersion,
	}
	for _, it := range s.downloadItems() {
		vm.Files = append(vm.Files, views.DownloadFileVM{
			Title: it.title,
			Desc:  it.desc,
			URL:   s.cfg.BasePath + "/downloads/" + it.id,
		})
	}
	s.render(w, r, views.Downloads(vm))
}

// download mints a short-lived presigned R2 URL for the requested item and
// redirects the browser to it, so the download streams from R2 to the client
// and never through this pod.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	item, ok := s.findDownload(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.presign == nil {
		http.Error(w, "Downloads are temporarily unavailable.", http.StatusServiceUnavailable)
		return
	}
	link, err := s.presign.presignGet(item.object, path.Base(item.object), s.cfg.R2PresignTTL)
	if err != nil {
		s.serverError(w, "presigning download", err)
		return
	}
	u := userFrom(r.Context())
	s.log.Info("download link issued", "by", u.DisplayName(), "object", item.object)
	http.Redirect(w, r, link, http.StatusFound)
}

func (s *Server) mint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)

	raw, hash := newToken()
	inv, err := s.store.CreateInvite(ctx, hash, u.Subject, u.DisplayName(), s.cfg.InviteTTL)
	if err != nil {
		s.serverError(w, "creating invite", err)
		return
	}
	s.log.Info("invite minted", "by", u.DisplayName(), "invite_id", inv.ID)

	link := strings.TrimRight(s.cfg.BaseURL.String(), "/") + "/i/" + raw
	s.render(w, r, views.MintResult(views.MintedVM{
		Link:      link,
		ExpiresAt: s.fmtTime(inv.ExpiresAt),
	}))
}

// cancelInvite revokes an unused invite and returns the updated table row so
// htmx can swap it in place. The `owner` query flag tells us whether the row is
// rendered with the "created by" column (admin all-invites view).
func (s *Server) cancelInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad invite id", http.StatusBadRequest)
		return
	}

	inv, err := s.store.CancelInvite(ctx, id, u.Subject, u.DisplayName(), u.IsAdmin())
	switch {
	case errors.Is(err, ErrInviteNotFound):
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	case errors.Is(err, ErrForbidden):
		http.Error(w, "you can only cancel your own invites", http.StatusForbidden)
		return
	case errors.Is(err, ErrInviteUsed):
		http.Error(w, "that invite was already used", http.StatusConflict)
		return
	case err != nil:
		s.serverError(w, "canceling invite", err)
		return
	}
	s.log.Info("invite canceled", "by", u.DisplayName(), "invite_id", id)

	showOwner := r.URL.Query().Get("owner") == "1"
	s.render(w, r, views.InviteRow(s.inviteRow(inv, u, showOwner)))
}

// playersHandler returns the online-players fragment (public, cached).
func (s *Server) playersHandler(w http.ResponseWriter, r *http.Request) {
	vm := views.PlayersVM{MapURL: s.cfg.MapURL}
	op, err := s.players.get(r.Context(), s.rcon.ListPlayers)
	if err != nil {
		s.log.Debug("player list unavailable", "err", err)
	} else {
		vm.Available = true
		vm.Online = op.Online
		vm.Max = op.Max
		vm.Names = op.Names
	}
	// Short client cache so many pollers do not each hit the app.
	w.Header().Set("Cache-Control", "public, max-age=10")
	s.render(w, r, views.Players(vm))
}

// whoami reports the current session's sign-in state as JSON. The landing page
// (mc-web, a static binary that cannot see this app's session cookie contents)
// fetches it to turn its "Sign in" link into the member's name when signed in.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		SignedIn bool   `json:"signedIn"`
		Name     string `json:"name,omitempty"`
	}{}
	if u, ok := s.auth.currentUser(r.Context()); ok {
		resp.SignedIn = true
		resp.Name = u.DisplayName()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) redeemForm(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	state := s.inviteState(r.Context(), token)
	s.render(w, r, views.Redeem(views.RedeemVM{
		Nav:       s.nav(User{}, true),
		State:     state,
		SubmitURL: s.cfg.BasePath + "/i/" + token,
	}))
}

func (s *Server) redeem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PathValue("token")
	submitURL := s.cfg.BasePath + "/i/" + token
	username := strings.TrimSpace(r.PostFormValue("username"))

	formErr := func(msg string) {
		s.render(w, r, views.Redeem(views.RedeemVM{
			Nav:       s.nav(User{}, true),
			State:     "form",
			SubmitURL: submitURL,
			Username:  username,
			Error:     msg,
		}))
	}

	if !s.limiter.allow(clientIP(r), time.Now()) {
		formErr("Too many attempts. Please wait a minute and try again.")
		return
	}

	profile, err := s.mojang.Resolve(ctx, username)
	switch {
	case errors.Is(err, ErrInvalidUsername):
		formErr("That is not a valid Minecraft Java username.")
		return
	case errors.Is(err, ErrUnknownPlayer):
		formErr("No Minecraft Java account has that name. Check the spelling and try again.")
		return
	case err != nil:
		s.log.Warn("mojang lookup failed", "err", err)
		formErr("Could not check that name right now. Please try again in a moment.")
		return
	}

	resp, err := s.store.RedeemInvite(ctx, hashToken(token), profile, func(ctx context.Context) (string, error) {
		return s.rcon.WhitelistAdd(ctx, profile.Name)
	})
	switch {
	case errors.Is(err, ErrInviteNotFound):
		s.renderState(w, r, "invalid", submitURL)
		return
	case errors.Is(err, ErrInviteUsed):
		s.renderState(w, r, "used", submitURL)
		return
	case errors.Is(err, ErrInviteCanceled):
		s.renderState(w, r, "canceled", submitURL)
		return
	case errors.Is(err, ErrInviteExpired):
		s.renderState(w, r, "expired", submitURL)
		return
	case err != nil:
		s.log.Error("redeem failed", "err", err, "player", profile.Name)
		formErr("The server could not be reached right now. Please try again in a minute.")
		return
	}

	s.log.Info("invite redeemed", "player", profile.Name, "uuid", profile.UUID, "rcon", resp)
	s.render(w, r, views.RedeemDone(views.RedeemDoneVM{
		Nav:           s.nav(User{}, true),
		MinecraftName: profile.Name,
		ServerAddress: s.cfg.ServerAddress,
	}))
}

func (s *Server) renderState(w http.ResponseWriter, r *http.Request, state, submitURL string) {
	s.render(w, r, views.Redeem(views.RedeemVM{
		Nav:       s.nav(User{}, true),
		State:     state,
		SubmitURL: submitURL,
	}))
}

// inviteState derives the redemption-page state from the stored invite.
func (s *Server) inviteState(ctx context.Context, token string) string {
	inv, err := s.store.FindInvite(ctx, hashToken(token))
	if errors.Is(err, ErrInviteNotFound) {
		return "invalid"
	}
	if err != nil {
		s.log.Warn("finding invite", "err", err)
		return "invalid"
	}
	switch st := inv.Status(time.Now()); st {
	case "used", "canceled", "expired":
		return st
	default:
		return "form"
	}
}

func (s *Server) nav(u User, public bool) views.NavVM {
	base := s.cfg.BasePath
	n := views.NavVM{
		HomeURL:      base + "/",
		DownloadsURL: base + "/downloads",
		LoginURL:     base + "/login",
		LogoutURL:    base + "/logout",
		LandingURL:   s.cfg.SiteURL,
		MapURL:       s.cfg.MapURL,
		HideAuth:     public,
	}
	if !public {
		n.HtmxSrc = base + "/assets/htmx.min.js"
	}
	if u.Subject != "" {
		n.SignedIn = true
		n.Name = u.DisplayName()
		n.IsAdmin = u.IsAdmin()
		n.IsInviter = u.IsInviter()
	}
	return n
}

func (s *Server) inviteRows(invites []Invite, u User, showOwner bool) []views.InviteRowVM {
	rows := make([]views.InviteRowVM, 0, len(invites))
	for _, inv := range invites {
		rows = append(rows, s.inviteRow(inv, u, showOwner))
	}
	return rows
}

func (s *Server) inviteRow(inv Invite, u User, showOwner bool) views.InviteRowVM {
	canCancel := inv.Cancelable() && (u.IsAdmin() || inv.CreatedBy == u.Subject)
	row := views.InviteRowVM{
		CreatedAt:     s.fmtTime(inv.CreatedAt),
		ExpiresAt:     s.fmtTime(inv.ExpiresAt),
		Status:        inv.Status(time.Now()),
		MinecraftName: inv.MinecraftName,
		ShowOwner:     showOwner,
		CanCancel:     canCancel,
	}
	if showOwner {
		row.CreatedBy = cmp.Or(inv.CreatedByName, inv.CreatedBy)
	}
	if canCancel {
		u := s.cfg.BasePath + "/invites/" + strconv.FormatInt(inv.ID, 10) + "/cancel"
		if showOwner {
			u += "?owner=1"
		}
		row.CancelURL = u
	}
	return row
}

func (s *Server) auditRows(entries []AuditEntry) []views.AuditRowVM {
	rows := make([]views.AuditRowVM, 0, len(entries))
	for _, e := range entries {
		action, detail := s.friendlyAudit(e)
		rows = append(rows, views.AuditRowVM{
			At:     s.fmtTime(e.At),
			Who:    cmp.Or(e.ActorName, e.Actor),
			Action: action,
			Detail: detail,
		})
	}
	return rows
}

// friendlyAudit turns a raw audit row into a human action + detail string.
func (s *Server) friendlyAudit(e AuditEntry) (action, detail string) {
	var d map[string]any
	_ = json.Unmarshal(e.Detail, &d)
	switch e.Action {
	case "invite_created":
		if exp, ok := d["expires_at"].(string); ok {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				return "Created an invite", "expires " + s.fmtTime(t)
			}
		}
		return "Created an invite", ""
	case "invite_redeemed":
		name, _ := d["minecraft_name"].(string)
		return "Joined the whitelist", name
	case "invite_canceled":
		return "Canceled an invite", ""
	default:
		return e.Action, string(e.Detail)
	}
}

func (s *Server) fmtTime(t time.Time) string {
	return t.In(s.loc).Format("Jan 2 2006, 3:04 PM")
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		s.log.Error("rendering component", "err", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	http.Error(w, "Something went wrong. Please try again.", http.StatusInternalServerError)
}

// playersCache serializes and caches the RCON `list` result so many pollers do
// not each trigger an RCON round-trip.
type playersCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	result    OnlinePlayers
	err       error
	fetchedAt time.Time
}

func (c *playersCache) get(ctx context.Context, fetch func(context.Context) (OnlinePlayers, error)) (OnlinePlayers, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl {
		return c.result, c.err
	}
	c.result, c.err = fetch(ctx)
	c.fetchedAt = time.Now()
	return c.result, c.err
}
