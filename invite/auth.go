package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Session keys. Auth-flow keys (state/pkce/nonce/return) are transient; the
// identity keys (sub/email/roles) persist for the logged-in session.
const (
	sessState    = "oidc_state"
	sessPKCE     = "oidc_pkce"
	sessNonce    = "oidc_nonce"
	sessReturnTo = "return_to"
	sessSubject  = "user_subject"
	sessEmail    = "user_email"
	sessName     = "user_name"  // display name (full name, else username, else email)
	sessRoles    = "user_roles" // comma-joined role list
)

type ctxKey int

const userCtxKey ctxKey = 0

// User is the authenticated identity carried on the request context.
type User struct {
	Subject string
	Email   string
	Name    string // display name for audit and "created by"
	roles   map[string]bool
}

// Has reports whether the user holds a role.
func (u User) Has(role string) bool { return u.roles[role] }

// IsAdmin reports the admin role.
func (u User) IsAdmin() bool { return u.roles["admin"] }

// IsInviter reports the inviter role (admins are always inviters too).
func (u User) IsInviter() bool { return u.roles["inviter"] }

// Auth wires OIDC (Authorization Code + PKCE) against Authentik to the session
// manager and exposes the login/callback/logout handlers and route guards.
type Auth struct {
	cfg      Config
	sessions *scs.SessionManager
	provider *oidc.Provider
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	log      *slog.Logger
}

// NewAuth performs OIDC discovery against the configured issuer and builds the
// relying-party client.
func NewAuth(ctx context.Context, cfg Config, sessions *scs.SessionManager, log *slog.Logger) (*Auth, error) {
	provider, err := oidc.NewProvider(ctx, cfg.OIDCIssuer)
	if err != nil {
		return nil, err
	}
	return &Auth{
		cfg:      cfg,
		sessions: sessions,
		provider: provider,
		oauth: &oauth2.Config{
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			// profile carries the groups claim in Authentik's default mapping;
			// email is for display. openid is mandatory.
			Scopes: []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID}),
		log:      log,
	}, nil
}

// Login starts the Authorization Code + PKCE flow: it stashes state, the PKCE
// verifier, and a nonce in the session, then redirects to Authentik.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	state := randToken()
	nonce := randToken()
	verifier := oauth2.GenerateVerifier()

	a.sessions.Put(r.Context(), sessState, state)
	a.sessions.Put(r.Context(), sessNonce, nonce)
	a.sessions.Put(r.Context(), sessPKCE, verifier)
	if rt := r.URL.Query().Get("return_to"); strings.HasPrefix(rt, a.cfg.BasePath+"/") {
		a.sessions.Put(r.Context(), sessReturnTo, rt)
	}

	url := a.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(nonce),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the flow: it validates state, exchanges the code with the
// PKCE verifier, verifies the ID token and nonce, maps groups to roles, and
// establishes the session. A user who authenticates but holds neither role is
// signed out with a 403 rather than given an empty session.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		a.log.Warn("oidc callback error", "error", errParam, "desc", r.URL.Query().Get("error_description"))
		http.Error(w, "Sign-in failed. Please try again.", http.StatusBadGateway)
		return
	}

	wantState := a.sessions.GetString(ctx, sessState)
	if wantState == "" || r.URL.Query().Get("state") != wantState {
		http.Error(w, "Sign-in expired or invalid. Please try again.", http.StatusBadRequest)
		return
	}
	verifier := a.sessions.GetString(ctx, sessPKCE)
	wantNonce := a.sessions.GetString(ctx, sessNonce)

	token, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		a.log.Warn("oidc code exchange failed", "err", err)
		http.Error(w, "Sign-in failed. Please try again.", http.StatusBadGateway)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "Sign-in failed: no id token.", http.StatusBadGateway)
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawID)
	if err != nil {
		a.log.Warn("id token verification failed", "err", err)
		http.Error(w, "Sign-in failed. Please try again.", http.StatusBadGateway)
		return
	}
	if idToken.Nonce != wantNonce {
		http.Error(w, "Sign-in failed: nonce mismatch.", http.StatusBadRequest)
		return
	}

	var claims struct {
		Email             string   `json:"email"`
		Name              string   `json:"name"`
		PreferredUsername string   `json:"preferred_username"`
		Groups            []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		a.log.Warn("reading id token claims", "err", err)
		http.Error(w, "Sign-in failed. Please try again.", http.StatusBadGateway)
		return
	}
	// Authentik returns groups from the ID token only when "Include claims in
	// id_token" is enabled; otherwise they are on the userinfo endpoint. Fall
	// back to userinfo so either provider configuration works.
	groups := claims.Groups
	if len(groups) == 0 {
		if ui, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(token)); err == nil {
			var uc struct {
				Email             string   `json:"email"`
				Name              string   `json:"name"`
				PreferredUsername string   `json:"preferred_username"`
				Groups            []string `json:"groups"`
			}
			if err := ui.Claims(&uc); err == nil {
				groups = uc.Groups
				claims.Email = cmp.Or(claims.Email, uc.Email)
				claims.Name = cmp.Or(claims.Name, uc.Name)
				claims.PreferredUsername = cmp.Or(claims.PreferredUsername, uc.PreferredUsername)
			}
		}
	}

	// A human-readable name for "created by" and the audit log: full name, else
	// username, else email, else the opaque subject as a last resort.
	displayName := cmp.Or(claims.Name, claims.PreferredUsername, claims.Email, idToken.Subject)

	// A user with no admin/inviter role is a guest: someone who self-registered
	// (Authentik enrollment lands them in mc-guest) but has not been granted
	// permissions yet. We still sign them in so they see a clear "pending"
	// page and so an admin has a real account to promote, rather than bouncing
	// them with a 403.
	roles := rolesFromGroups(groups, a.cfg.AdminGroup, a.cfg.InviterGroup)
	if len(roles) == 0 {
		a.log.Info("authenticated guest with no role yet", "subject", idToken.Subject, "groups", groups)
	}

	// New session token on privilege change, to prevent session fixation.
	if err := a.sessions.RenewToken(ctx); err != nil {
		a.log.Error("renewing session", "err", err)
		http.Error(w, "Sign-in failed. Please try again.", http.StatusInternalServerError)
		return
	}
	a.sessions.Put(ctx, sessSubject, idToken.Subject)
	a.sessions.Put(ctx, sessEmail, claims.Email)
	a.sessions.Put(ctx, sessName, displayName)
	a.sessions.Put(ctx, sessRoles, strings.Join(roles, ","))
	a.sessions.Remove(ctx, sessState)
	a.sessions.Remove(ctx, sessPKCE)
	a.sessions.Remove(ctx, sessNonce)

	dest := a.sessions.PopString(ctx, sessReturnTo)
	if dest == "" {
		dest = a.cfg.BasePath + "/"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// Logout destroys the session.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if err := a.sessions.Destroy(r.Context()); err != nil {
		a.log.Error("destroying session", "err", err)
	}
	http.Redirect(w, r, a.cfg.BasePath+"/", http.StatusFound)
}

// currentUser reconstructs the User from the session, if signed in.
func (a *Auth) currentUser(ctx context.Context) (User, bool) {
	sub := a.sessions.GetString(ctx, sessSubject)
	if sub == "" {
		return User{}, false
	}
	roles := map[string]bool{}
	for _, r := range strings.Split(a.sessions.GetString(ctx, sessRoles), ",") {
		if r != "" {
			roles[r] = true
		}
	}
	return User{
		Subject: sub,
		Email:   a.sessions.GetString(ctx, sessEmail),
		Name:    a.sessions.GetString(ctx, sessName),
		roles:   roles,
	}, true
}

// DisplayName is the user's name, falling back to email then subject.
func (u User) DisplayName() string {
	return cmp.Or(u.Name, u.Email, u.Subject)
}

// requireAuth admits signed-in users, attaching the User to the context, and
// redirects everyone else to the login flow with a return-to.
func (a *Auth) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.currentUser(r.Context())
		if !ok {
			http.Redirect(w, r, a.cfg.BasePath+"/login?return_to="+r.URL.Path, http.StatusFound)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
	}
}

// requireRole is requireAuth plus a role check (admin satisfies any role).
func (a *Auth) requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r.Context())
		if !u.Has(role) {
			http.Error(w, "You do not have permission for this.", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// userFrom returns the User attached by requireAuth. Handlers behind that
// middleware can rely on it; the zero User is returned otherwise.
func userFrom(ctx context.Context) User {
	u, _ := ctx.Value(userCtxKey).(User)
	return u
}

// rolesFromGroups maps Authentik group names to app roles. Admin implies
// inviter, so an admin can do everything an inviter can.
func rolesFromGroups(groups []string, adminGroup, inviterGroup string) []string {
	var isAdmin, isInviter bool
	for _, g := range groups {
		switch g {
		case adminGroup:
			isAdmin = true
		case inviterGroup:
			isInviter = true
		}
	}
	var roles []string
	if isAdmin {
		roles = append(roles, "admin")
	}
	if isAdmin || isInviter {
		roles = append(roles, "inviter")
	}
	return roles
}

func randToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
