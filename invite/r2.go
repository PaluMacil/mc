package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// presigner turns an object key into a time-limited URL that a browser can GET
// directly from R2, so multi-gigabyte downloads never stream through this pod
// (it has a read-only root filesystem and only ReadHeaderTimeout set). It is an
// interface so the download handlers can be tested without R2 credentials.
type presigner interface {
	presignGet(objectKey, downloadName string, expires time.Duration) (string, error)
}

// r2Presigner builds AWS Signature Version 4 query-string ("presigned") GET URLs
// for a Cloudflare R2 bucket. R2 speaks the S3 API with path-style addressing
// and the region literal "auto". Presigning is a local HMAC computation: it
// makes no network call and needs only a read-capable access key.
type r2Presigner struct {
	endpoint  string // https://<account>.r2.cloudflarestorage.com
	bucket    string
	accessKey string
	secretKey string
}

const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	sigV4Region     = "auto" // R2 accepts the literal "auto"
	sigV4Service    = "s3"
	sigV4Terminator = "aws4_request"
	unsignedPayload = "UNSIGNED-PAYLOAD"
)

func (p r2Presigner) presignGet(objectKey, downloadName string, expires time.Duration) (string, error) {
	u, err := url.Parse(p.endpoint)
	if err != nil {
		return "", fmt.Errorf("parsing R2 endpoint %q: %w", p.endpoint, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("R2 endpoint %q has no host", p.endpoint)
	}
	// Path-style key: /<bucket>/<object>. Encode each segment, keep the slashes.
	canonicalURI := "/" + awsURIEncode(p.bucket, false) + "/" + awsURIEncode(objectKey, false)

	extra := map[string]string{}
	if downloadName != "" {
		// Ask R2 to serve it as an attachment with a clean filename regardless of
		// the object's stored metadata.
		extra["response-content-disposition"] = `attachment; filename="` + downloadName + `"`
	}

	sig, query := presignQueryV4(presignInput{
		host:         u.Host,
		canonicalURI: canonicalURI,
		accessKey:    p.accessKey,
		secretKey:    p.secretKey,
		region:       sigV4Region,
		service:      sigV4Service,
		expires:      expires,
		now:          time.Now().UTC(),
		extra:        extra,
	})
	return u.Scheme + "://" + u.Host + canonicalURI + "?" + query + "&X-Amz-Signature=" + sig, nil
}

// presignInput is the fully-specified input to the SigV4 presign computation.
// It is separated from r2Presigner so the algorithm can be exercised against
// AWS's published known-answer vector (fixed host, region and clock) in tests.
type presignInput struct {
	host         string
	canonicalURI string // already AWS-URI-encoded, with a leading slash
	accessKey    string
	secretKey    string
	region       string
	service      string
	expires      time.Duration
	now          time.Time
	extra        map[string]string // additional query params to sign (unencoded)
}

// presignQueryV4 returns the hex signature and the canonical (signed) query
// string, which excludes the trailing X-Amz-Signature parameter.
func presignQueryV4(in presignInput) (signature, canonicalQuery string) {
	datestamp := in.now.Format("20060102")
	amzDate := in.now.Format("20060102T150405Z")
	scope := datestamp + "/" + in.region + "/" + in.service + "/" + sigV4Terminator

	q := map[string]string{
		"X-Amz-Algorithm":     sigV4Algorithm,
		"X-Amz-Credential":    in.accessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(in.expires.Seconds())),
		"X-Amz-SignedHeaders": "host",
	}
	maps.Copy(q, in.extra)
	canonicalQuery = canonicalQueryString(q)

	// CanonicalRequest = METHOD \n URI \n QUERY \n CanonicalHeaders \n
	// SignedHeaders \n HashedPayload. CanonicalHeaders already ends in \n, so the
	// join adds the extra blank line the spec requires before SignedHeaders.
	canonicalRequest := strings.Join([]string{
		"GET",
		in.canonicalURI,
		canonicalQuery,
		"host:" + in.host + "\n",
		"host",
		unsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	key := sigV4SigningKey(in.secretKey, datestamp, in.region, in.service)
	signature = hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
	return signature, canonicalQuery
}

// canonicalQueryString URI-encodes every key and value and joins them sorted by
// encoded key, as the SigV4 canonical query rules require.
func canonicalQueryString(q map[string]string) string {
	pairs := make([]string, 0, len(q))
	for k, v := range q {
		pairs = append(pairs, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
	}
	sort.Strings(pairs) // keys are distinct, so pair order == key order
	return strings.Join(pairs, "&")
}

func sigV4SigningKey(secret, datestamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(sigV4Terminator))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// awsURIEncode percent-encodes per RFC 3986 the way SigV4 requires: every byte
// except the unreserved set A-Za-z0-9-_.~ is encoded; '/' is left as-is only
// when encodeSlash is false (for the object path). Encoding byte-wise keeps
// multibyte UTF-8 correct.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
