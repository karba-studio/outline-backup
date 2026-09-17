package s3

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestSignKnownVector checks the signer against the worked example published in
// the AWS "Signature Calculations for the Authorization Header" documentation
// (GET Bucket Lifecycle). It exercises the whole chain: canonical request,
// string to sign, and the four-step key derivation.
//
// If this test ever fails, the signer is wrong. Do not adjust the expectation.
func TestSignKnownVector(t *testing.T) {
	c := &Client{
		Endpoint:  "https://examplebucket.s3.amazonaws.com",
		Region:    "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?lifecycle", nil)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	c.sign(req, map[string]string{"lifecycle": ""}, EmptyPayloadHash, when)

	const want = "AWS4-HMAC-SHA256 " +
		"Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
		"Signature=fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"

	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization header mismatch\n got: %s\nwant: %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20130524T000000Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
}

func TestURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		// Outline names attachments after the uploaded file, so spaces are
		// routine and were the first thing to break a naive implementation.
		{"2026-09-08 15.11.20.jpg", true, "2026-09-08%2015.11.20.jpg"},
		{"uploads/a/b.png", false, "uploads/a/b.png"},
		{"uploads/a/b.png", true, "uploads%2Fa%2Fb.png"},
		{"~tilde-stays", true, "~tilde-stays"},
		{"plus+sign", true, "plus%2Bsign"},
		{"a=b&c", true, "a%3Db%26c"},
		{"ünïcode", true, "%C3%BCn%C3%AFcode"},
		{"", true, ""},
	}
	for _, tc := range cases {
		if got := uriEncode(tc.in, tc.encodeSlash); got != tc.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", tc.in, tc.encodeSlash, got, tc.want)
		}
	}
}

func TestCanonicalQuerySortsAndEncodes(t *testing.T) {
	got := canonicalQuery(map[string]string{
		"list-type":          "2",
		"continuation-token": "a+b/c=",
		"prefix":             "up loads/",
	})
	const want = "continuation-token=a%2Bb%2Fc%3D&list-type=2&prefix=up%20loads%2F"
	if got != want {
		t.Errorf("canonicalQuery =\n %s\nwant\n %s", got, want)
	}
}

func TestSafeRelPathRejectsTraversal(t *testing.T) {
	hostile := []string{
		"../../etc/passwd",
		"/../../etc/passwd",
		`..\..\windows\system32`,
		"a/../../b",
		"",
		"/",
	}
	for _, key := range hostile {
		if _, err := safeRelPath(key); err == nil {
			t.Errorf("safeRelPath(%q) was accepted; it must be rejected", key)
		}
	}

	ok := map[string]string{
		"uploads/team/id/file.png": "uploads/team/id/file.png",
		"/leading/slash.txt":       "leading/slash.txt",
		"a/./b.txt":                "a/b.txt",
	}
	for key, want := range ok {
		got, err := safeRelPath(key)
		if err != nil {
			t.Errorf("safeRelPath(%q) errored: %v", key, err)
			continue
		}
		if filepathToSlash(got) != want {
			t.Errorf("safeRelPath(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1024:    "1.0 KB",
		1536:    "1.5 KB",
		1 << 20: "1.0 MB",
		1 << 30: "1.0 GB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// filepathToSlash keeps the traversal test readable on both path flavours.
func filepathToSlash(p string) string { return filepath.ToSlash(p) }
