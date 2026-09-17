// Package s3 is a minimal AWS Signature Version 4 client, enough for the handful
// of S3 operations this tool needs against MinIO.
//
// It exists so the binary has no third-party dependencies. The signing
// implementation is deliberately literal: it follows the AWS specification step
// by step rather than optimising, because a subtle bug here shows up as an
// opaque 403 at the worst possible moment.
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	algorithm       = "AWS4-HMAC-SHA256"
	service         = "s3"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	// EmptyPayloadHash is sha256 of the empty string, required on bodyless requests.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	isoLayout  = "20060102T150405Z"
	dateLayout = "20060102"
)

// uriEncode percent-encodes per RFC 3986 as AWS requires: unreserved characters
// pass through, everything else becomes uppercase percent-hex. Slashes are kept
// literal in paths and encoded in query values.
func uriEncode(s string, encodeSlash bool) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case strings.IndexByte(unreserved, c) >= 0:
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// encodePath encodes each segment of an object path, keeping separators.
func encodePath(p string) string {
	if p == "" {
		return "/"
	}
	return uriEncode(p, false)
}

// canonicalQuery renders query parameters in the order AWS expects.
func canonicalQuery(q map[string]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k, true)+"="+uriEncode(q[k], true))
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// signingKey derives the scoped key for a given day and region.
func signingKey(secret, datestamp, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), datestamp)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// sign adds the Authorization, X-Amz-Date and X-Amz-Content-Sha256 headers to req.
//
// payloadHash must be the hex sha256 of the body, EmptyPayloadHash for a bodyless
// request, or unsignedPayload when the body cannot be hashed up front (only used
// over HTTPS, where the transport already protects integrity).
func (c *Client) sign(req *http.Request, query map[string]string, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format(isoLayout)
	datestamp := now.UTC().Format(dateLayout)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	// Canonical headers: every header we intend to sign, lowercased and sorted.
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	values := map[string]string{
		"host":                 req.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		signed = append(signed, "content-type")
		values["content-type"] = ct
	}
	sort.Strings(signed)

	var canonHeaders strings.Builder
	for _, h := range signed {
		canonHeaders.WriteString(h)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(values[h]))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(signed, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		encodePath(req.URL.Path),
		canonicalQuery(query),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := datestamp + "/" + c.Region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, datestamp, c.Region), stringToSign))

	req.Header.Set("Authorization", algorithm+
		" Credential="+c.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+sig)
}
