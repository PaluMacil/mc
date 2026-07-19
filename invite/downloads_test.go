package main

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PaluMacil/mc/invite/views"
	"github.com/stretchr/testify/require"
)

// TestPresignKnownAnswer checks the SigV4 presign computation against the
// worked example in the AWS docs ("Authenticating Requests: Using Query
// Parameters"), so a regression in canonicalization or the signing key is
// caught without needing real credentials.
func TestPresignKnownAnswer(t *testing.T) {
	sig, query := presignQueryV4(presignInput{
		host:         "examplebucket.s3.amazonaws.com",
		canonicalURI: "/test.txt",
		accessKey:    "AKIAIOSFODNN7EXAMPLE",
		secretKey:    "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:       "us-east-1",
		service:      "s3",
		expires:      86400 * time.Second,
		now:          time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
	})
	require.Equal(t, "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", sig)
	require.Contains(t, query, "X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request")
	require.Contains(t, query, "X-Amz-SignedHeaders=host")
}

func TestAWSURIEncode(t *testing.T) {
	require.Equal(t, "atm10-7.1-client.zip", awsURIEncode("atm10-7.1-client.zip", true))
	require.Equal(t, "a%20b", awsURIEncode("a b", true))
	require.Equal(t, "mods/x", awsURIEncode("mods/x", false))
	require.Equal(t, "mods%2Fx", awsURIEncode("mods/x", true))
	require.Equal(t, "%22%3B", awsURIEncode(`";`, true))
}

func TestR2PresignGetShape(t *testing.T) {
	p := r2Presigner{
		endpoint:  "https://acct.r2.cloudflarestorage.com",
		bucket:    "mc-mods",
		accessKey: "AKID",
		secretKey: "secret",
	}
	u, err := p.presignGet("atm10-7.1-client.zip", "atm10-7.1-client.zip", time.Hour)
	require.NoError(t, err)
	require.Contains(t, u, "https://acct.r2.cloudflarestorage.com/mc-mods/atm10-7.1-client.zip?")
	require.Contains(t, u, "X-Amz-Signature=")
	require.Contains(t, u, "response-content-disposition=")
}

func TestDownloadsRequiresAuth(t *testing.T) {
	srv := newStatelessTestServer(t, &stubWhitelist{})
	code, _ := do(t, srv.Handler(), http.MethodGet, "/invite/downloads", "")
	require.Equal(t, http.StatusFound, code, "unauthenticated downloads redirects to login")
}

func TestNavAdminDropdown(t *testing.T) {
	render := func(nav views.NavVM) string {
		var buf bytes.Buffer
		require.NoError(t, views.Pending(nav).Render(context.Background(), &buf))
		return buf.String()
	}
	base := views.NavVM{
		LandingURL: "/", MapURL: "/map/", TipsURL: "/tips", ParentsURL: "/parents",
		HomeURL: "/portal/", DownloadsURL: "/portal/downloads", LogoutURL: "/portal/logout",
		LoginURL: "/portal/login", MetricsURL: "https://grafana.example/d/mc",
	}

	admin := base
	admin.SignedIn, admin.Name, admin.IsAdmin = true, "Palu", true
	out := render(admin)
	require.Contains(t, out, `class="usermenu"`)
	require.Contains(t, out, "Danland") // brand
	require.Contains(t, out, "Metrics dashboard")
	require.Contains(t, out, "grafana.example/d/mc")
	require.Contains(t, out, "ATM10")  // Tips dropdown item (public)
	require.Contains(t, out, "Invite") // member link (renamed from Dashboard)
	require.Contains(t, out, "Downloads")

	guest := base
	guest.SignedIn, guest.Name = true, "Guest"
	out = render(guest)
	require.Contains(t, out, "Sign out")
	require.NotContains(t, out, "Metrics dashboard") // admin-only

	out = render(base) // signed out
	require.Contains(t, out, "Sign in")
	require.NotContains(t, out, `class="usermenu"`)
}

func TestDownloadsPageRenders(t *testing.T) {
	vm := views.DownloadsVM{
		Nav:           views.NavVM{SignedIn: true},
		Available:     true,
		CurseforgeURL: "/curseforge",
		VanillaURL:    "/vanilla",
		Client: []views.DownloadFileVM{
			{Title: "atm10-7.1-client.zip", Size: "1.5 GB", URL: "/invite/downloads/get?o=atm10-7.1-client.zip"},
		},
		Server: []views.DownloadFileVM{
			{Title: "ServerFiles-7.1.zip", Size: "1.1 GB", URL: "/invite/downloads/get?o=ServerFiles-7.1.zip"},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, views.Downloads(vm).Render(context.Background(), &buf))
	body := buf.String()
	require.Contains(t, body, "atm10-7.1-client.zip")     // client pack row
	require.Contains(t, body, "Server files")             // server section header
	require.Contains(t, body, "CurseForge install guide") // intro link
	require.Contains(t, body, "downloads/get?o=")         // presign endpoint
	require.Contains(t, body, "1.5 GB")                   // size
}

func TestHumanWho(t *testing.T) {
	require.Equal(t, "dcwolf@gmail.com", humanWho("dcwolf@gmail.com"))
	require.Equal(t, "Dan Wolf", humanWho("Dan Wolf"))
	require.Equal(t, "Palu_Macil", humanWho("Palu_Macil"))
	require.Equal(t, "unknown", humanWho(""))
	require.Equal(t, "id:1646ac34…",
		humanWho("1646ac34d5c11fe678b5a7e34418aa57cf3a882ceccb5ba59f5d9f6be1b75fd1"))
}

func TestShortSubject(t *testing.T) {
	require.Equal(t, "short", shortSubject("short"))
	require.Equal(t, "id:1646ac34…",
		shortSubject("1646ac34d5c11fe678b5a7e34418aa57cf3a882ceccb5ba59f5d9f6be1b75fd1"))
}

func TestHumanBytes(t *testing.T) {
	require.Equal(t, "512 B", humanBytes(512))
	require.Equal(t, "1.0 KB", humanBytes(1024))
	require.Equal(t, "1.5 GB", humanBytes(1610612736))
}
