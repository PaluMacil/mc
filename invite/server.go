package main

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/PaluMacil/mc/invite/views"
	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"
)

//go:embed assets/htmx.min.js
var htmxJS []byte

// whitelistAdder grants a player access over RCON. It is an interface at the
// consumer so tests can substitute a stub for the real *RCONClient.
type whitelistAdder interface {
	WhitelistAdd(ctx context.Context, name string) (string, error)
}

// Server holds the wired dependencies and serves the app.
type Server struct {
	cfg       Config
	store     *Store
	auth      *Auth
	sessions  *scs.SessionManager
	mojang    MojangResolver
	whitelist whitelistAdder
	limiter   *ipLimiter
	loc       *time.Location
	log       *slog.Logger
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

	// Inviter/admin dashboard.
	mux.HandleFunc("GET "+base+"/{$}", s.auth.requireAuth(s.home))
	mux.HandleFunc("GET "+base+"/login", s.auth.Login)
	mux.HandleFunc("GET "+base+"/auth/callback", s.auth.Callback)
	mux.HandleFunc("POST "+base+"/logout", s.auth.Logout)
	mux.HandleFunc("POST "+base+"/invites", s.auth.requireRole("inviter", s.mint))

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
	// methods. Same-origin form posts (mint, logout, redeem) pass; the only
	// cross-site entry, the OIDC callback, is a GET and is not checked.
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
		Nav:       s.nav(u, false),
		CanMint:   u.IsInviter(),
		MintURL:   s.cfg.BasePath + "/invites",
		AdminView: u.IsAdmin(),
		ShowAll:   showAll,
		ShowOwner: showAll,
		Invites:   s.inviteRows(invites, showAll),
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

func (s *Server) mint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)

	raw, hash := newToken()
	inv, err := s.store.CreateInvite(ctx, hash, u.Subject, s.cfg.InviteTTL)
	if err != nil {
		s.serverError(w, "creating invite", err)
		return
	}
	s.log.Info("invite minted", "by", u.Subject, "invite_id", inv.ID)

	link := strings.TrimRight(s.cfg.BaseURL.String(), "/") + "/i/" + raw
	s.render(w, r, views.MintResult(views.MintedVM{
		Link:      link,
		ExpiresAt: s.fmtTime(inv.ExpiresAt),
	}))
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
		return s.whitelist.WhitelistAdd(ctx, profile.Name)
	})
	switch {
	case errors.Is(err, ErrInviteNotFound):
		s.renderState(w, r, "invalid", submitURL)
		return
	case errors.Is(err, ErrInviteUsed):
		s.renderState(w, r, "used", submitURL)
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
	switch inv.Status(time.Now()) {
	case "used":
		return "used"
	case "expired":
		return "expired"
	default:
		return "form"
	}
}

func (s *Server) nav(u User, public bool) views.NavVM {
	base := s.cfg.BasePath
	n := views.NavVM{
		HomeURL:   base + "/",
		LoginURL:  base + "/login",
		LogoutURL: base + "/logout",
		HideAuth:  public,
	}
	if !public {
		n.HtmxSrc = base + "/assets/htmx.min.js"
	}
	if u.Subject != "" {
		n.SignedIn = true
		n.Email = u.Email
		n.IsAdmin = u.IsAdmin()
		n.IsInviter = u.IsInviter()
	}
	return n
}

func (s *Server) inviteRows(invites []Invite, withOwner bool) []views.InviteRowVM {
	now := time.Now()
	rows := make([]views.InviteRowVM, 0, len(invites))
	for _, inv := range invites {
		row := views.InviteRowVM{
			CreatedAt:     s.fmtTime(inv.CreatedAt),
			ExpiresAt:     s.fmtTime(inv.ExpiresAt),
			Status:        inv.Status(now),
			MinecraftName: inv.MinecraftName,
		}
		if withOwner {
			row.CreatedBy = inv.CreatedBy
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) auditRows(entries []AuditEntry) []views.AuditRowVM {
	rows := make([]views.AuditRowVM, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, views.AuditRowVM{
			At:     s.fmtTime(e.At),
			Actor:  e.Actor,
			Action: e.Action,
			Detail: string(e.Detail),
		})
	}
	return rows
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
