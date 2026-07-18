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

func TestDownloadsPageRenders(t *testing.T) {
	vm := views.DownloadsVM{
		Nav:           views.NavVM{SignedIn: true, DownloadsURL: "/invite/downloads"},
		Available:     true,
		ServerAddress: "mc.danwolf.net",
		FallbackAddr:  "game.danwolf.net:25999",
		NeoForge:      "21.1.234",
		PackVersion:   "7.1",
		Files: []views.DownloadFileVM{
			{Title: "ATM10 client pack (7.1)", Desc: "desc", URL: "/invite/downloads/client"},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, views.Downloads(vm).Render(context.Background(), &buf))
	body := buf.String()
	require.Contains(t, body, "/invite/downloads/client")
	require.Contains(t, body, "21.1.234")
	require.Contains(t, body, "mc.danwolf.net")
}
