package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration, read from INVITE_*
// environment variables. Secrets (OIDC client secret, DB URL, RCON password)
// arrive the same way; the Deployment wires them from Kubernetes Secrets.
type Config struct {
	ListenAddr string

	// BaseURL is the absolute public URL the app is served from, including any
	// subpath (e.g. https://mc.danwolf.net/invite). BasePath and RedirectURL
	// are derived from it so routes, cookie Path, and the OIDC redirect all
	// agree with the Ingress and the Authentik client registration.
	BaseURL     *url.URL
	BasePath    string
	RedirectURL string

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	AdminGroup       string
	InviterGroup     string

	RCONAddr     string
	RCONPassword string

	DatabaseURL string

	InviteTTL time.Duration

	// ServerAddress is shown to a redeemed invitee so they know where to
	// connect; it is not the app's own address.
	ServerAddress string

	// TZ is the IANA timezone the dashboard formats timestamps in. The tz
	// database is embedded (time/tzdata), so the image needs no tzdata package.
	TZ string

	// SiteURL and MapURL are where the landing page and live map live, for the
	// portal's nav. Same host, so relative paths by default.
	SiteURL string
	MapURL  string

	// TipsURL and ParentsURL are the landing-site tip pages, shown in the shared
	// nav so the portal carries the same links as the public pages. MetricsURL is
	// the Grafana dashboard, linked from the admin-only user dropdown.
	TipsURL       string
	ParentsURL    string
	RulesURL      string
	CurseforgeURL string
	VanillaURL    string
	MetricsURL    string

	// R2 (Cloudflare) settings for the authenticated Downloads page. The app
	// mints short-lived presigned GET URLs and redirects the browser straight to
	// R2, so the large client-pack download never streams through this pod.
	// Endpoint and bucket are non-secret; the two keys come from the mc-r2
	// Secret (read-only). If any are unset the Downloads page shows as
	// unavailable rather than blocking the rest of the portal from starting.
	R2Endpoint        string
	R2Bucket          string
	R2AccessKeyID     string
	R2SecretAccessKey string

	// ClientPackObject is the R2 object name of the ready-to-play client bundle
	// offered on the Downloads page. R2PresignTTL bounds how long a minted link
	// stays valid to START a download; the transfer itself may run past it.
	ClientPackObject string
	R2PresignTTL     time.Duration
}

// LoadConfig reads and validates configuration from the environment. It returns
// a joined error naming every missing or malformed setting at once, so a
// misconfigured Deployment fails fast with a complete list rather than one
// field per restart.
func LoadConfig() (Config, error) {
	c := Config{
		ListenAddr:        envOr("INVITE_LISTEN_ADDR", ":8080"),
		OIDCIssuer:        os.Getenv("INVITE_OIDC_ISSUER"),
		OIDCClientID:      os.Getenv("INVITE_OIDC_CLIENT_ID"),
		OIDCClientSecret:  os.Getenv("INVITE_OIDC_CLIENT_SECRET"),
		AdminGroup:        envOr("INVITE_ADMIN_GROUP", "mc-admin"),
		InviterGroup:      envOr("INVITE_INVITER_GROUP", "mc-inviter"),
		RCONAddr:          os.Getenv("INVITE_RCON_ADDR"),
		RCONPassword:      os.Getenv("INVITE_RCON_PASSWORD"),
		DatabaseURL:       os.Getenv("INVITE_DATABASE_URL"),
		ServerAddress:     envOr("INVITE_SERVER_ADDRESS", "mc.danwolf.net"),
		TZ:                envOr("INVITE_TZ", "America/Chicago"),
		SiteURL:           envOr("INVITE_SITE_URL", "/"),
		MapURL:            envOr("INVITE_MAP_URL", "/map/"),
		TipsURL:           envOr("INVITE_TIPS_URL", "/tips"),
		ParentsURL:        envOr("INVITE_PARENTS_URL", "/parents"),
		RulesURL:          envOr("INVITE_RULES_URL", "/rules"),
		CurseforgeURL:     envOr("INVITE_CURSEFORGE_URL", "/curseforge"),
		VanillaURL:        envOr("INVITE_VANILLA_URL", "/vanilla"),
		MetricsURL:        envOr("INVITE_METRICS_URL", "https://grafana.danwolf.net/d/mc-atm10"),
		R2Endpoint:        os.Getenv("INVITE_R2_ENDPOINT"),
		R2Bucket:          os.Getenv("INVITE_R2_BUCKET"),
		R2AccessKeyID:     os.Getenv("INVITE_R2_ACCESS_KEY_ID"),
		R2SecretAccessKey: os.Getenv("INVITE_R2_SECRET_ACCESS_KEY"),
		ClientPackObject:  envOr("INVITE_CLIENT_PACK_OBJECT", "atm10-7.1-client.zip"),
	}

	var errs []error

	rawBase := os.Getenv("INVITE_BASE_URL")
	if rawBase == "" {
		errs = append(errs, errors.New("INVITE_BASE_URL is required"))
	} else if u, err := url.Parse(rawBase); err != nil {
		errs = append(errs, fmt.Errorf("parsing INVITE_BASE_URL: %w", err))
	} else if u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("INVITE_BASE_URL %q must be absolute (scheme and host)", rawBase))
	} else {
		c.BaseURL = u
		c.BasePath = strings.TrimRight(u.Path, "/")
		c.RedirectURL = strings.TrimRight(rawBase, "/") + "/auth/callback"
	}

	ttl := envOr("INVITE_TTL", "168h")
	if d, err := time.ParseDuration(ttl); err != nil {
		errs = append(errs, fmt.Errorf("parsing INVITE_TTL %q: %w", ttl, err))
	} else if d <= 0 {
		errs = append(errs, fmt.Errorf("INVITE_TTL %q must be positive", ttl))
	} else {
		c.InviteTTL = d
	}

	presignTTL := envOr("INVITE_R2_PRESIGN_TTL", "1h")
	if d, err := time.ParseDuration(presignTTL); err != nil {
		errs = append(errs, fmt.Errorf("parsing INVITE_R2_PRESIGN_TTL %q: %w", presignTTL, err))
	} else if d <= 0 {
		errs = append(errs, fmt.Errorf("INVITE_R2_PRESIGN_TTL %q must be positive", presignTTL))
	} else {
		c.R2PresignTTL = d
	}

	for name, v := range map[string]string{
		"INVITE_OIDC_ISSUER":        c.OIDCIssuer,
		"INVITE_OIDC_CLIENT_ID":     c.OIDCClientID,
		"INVITE_OIDC_CLIENT_SECRET": c.OIDCClientSecret,
		"INVITE_RCON_ADDR":          c.RCONAddr,
		"INVITE_RCON_PASSWORD":      c.RCONPassword,
		"INVITE_DATABASE_URL":       c.DatabaseURL,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
