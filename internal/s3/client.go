package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client speaks just enough S3 to move Outline's attachments in and out.
type Client struct {
	Endpoint  string // e.g. https://s3.example.com
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool

	HTTP *http.Client
}

// New builds a client with sensible timeouts. Attachments can be large, so the
// overall timeout is generous while the connection handshake is not.
func New(endpoint, region, accessKey, secretKey string, pathStyle bool) *Client {
	if region == "" {
		region = "us-east-1"
	}
	return &Client{
		Endpoint:  strings.TrimRight(endpoint, "/"),
		Region:    region,
		AccessKey: accessKey,
		SecretKey: secretKey,
		PathStyle: pathStyle,
		HTTP: &http.Client{
			Timeout: 30 * time.Minute,
		},
	}
}

// Object is one entry in a bucket listing.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Error is a non-2xx response, with whatever S3 told us about why.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	Resource   string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("s3: %s (%s) %s", e.Code, http.StatusText(e.StatusCode), e.Message)
	}
	return fmt.Sprintf("s3: HTTP %d", e.StatusCode)
}

// NotFound reports whether the error means the object or bucket is absent.
func (e *Error) NotFound() bool {
	return e.StatusCode == http.StatusNotFound ||
		e.Code == "NoSuchKey" || e.Code == "NoSuchBucket"
}

func parseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{StatusCode: resp.StatusCode}
	var payload struct {
		Code     string `xml:"Code"`
		Message  string `xml:"Message"`
		Resource string `xml:"Resource"`
	}
	if err := xml.Unmarshal(body, &payload); err == nil {
		e.Code, e.Message, e.Resource = payload.Code, payload.Message, payload.Resource
	}
	if e.Message == "" && len(body) > 0 {
		e.Message = strings.TrimSpace(string(body))
	}
	return e
}

// url builds the request URL for a bucket and key.
func (c *Client) url(bucket, key string, query map[string]string) (*url.URL, error) {
	base, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint %q: %w", c.Endpoint, err)
	}
	var p string
	if c.PathStyle || bucket == "" {
		p = "/" + bucket
		if key != "" {
			p += "/" + key
		}
	} else {
		base.Host = bucket + "." + base.Host
		p = "/" + key
	}
	if p == "" {
		p = "/"
	}
	// Path holds the decoded form; RawPath holds our AWS-style encoding. Go
	// emits RawPath verbatim as long as it decodes back to Path, which keeps the
	// bytes on the wire identical to the bytes we signed.
	base.Path = p
	base.RawPath = encodePath(p)

	// Likewise the query: url.Values.Encode would render a space as "+" and
	// leave "~" alone, both of which disagree with AWS canonical form and would
	// produce a signature mismatch on any key containing a space.
	base.RawQuery = canonicalQuery(query)
	return base, nil
}

func (c *Client) do(ctx context.Context, method, bucket, key string, query map[string]string,
	body io.Reader, payloadHash string, contentLength int64, contentType string) (*http.Response, error) {

	u, err := c.url(bucket, key, query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.sign(req, query, payloadHash, time.Now())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, parseError(resp)
	}
	return resp, nil
}

type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	EncodingType          string   `xml:"EncodingType"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		ETag         string    `xml:"ETag"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

// ListObjects returns every object under prefix, following pagination.
//
// The listing asks for url encoding-type so that keys containing characters XML
// cannot carry (and keys with spaces, which Outline produces from filenames like
// "2026-09-08 15.11.20.jpg") survive the round trip intact.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := map[string]string{
			"list-type":     "2",
			"max-keys":      "1000",
			"encoding-type": "url",
		}
		if prefix != "" {
			q["prefix"] = prefix
		}
		if token != "" {
			q["continuation-token"] = token
		}
		resp, err := c.do(ctx, http.MethodGet, bucket, "", q, nil, EmptyPayloadHash, 0, "")
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var lr listResult
		if err := xml.Unmarshal(body, &lr); err != nil {
			return nil, fmt.Errorf("parsing bucket listing: %w", err)
		}
		for _, item := range lr.Contents {
			key := item.Key
			if strings.EqualFold(lr.EncodingType, "url") {
				if dec, err := url.QueryUnescape(key); err == nil {
					key = dec
				}
			}
			// Directory placeholders carry no data and would create empty files.
			if strings.HasSuffix(key, "/") && item.Size == 0 {
				continue
			}
			out = append(out, Object{
				Key:          key,
				Size:         item.Size,
				ETag:         strings.Trim(item.ETag, `"`),
				LastModified: item.LastModified,
			})
		}
		if !lr.IsTruncated || lr.NextContinuationToken == "" {
			return out, nil
		}
		token = lr.NextContinuationToken
	}
}

// GetObject opens an object for reading. The caller closes the reader.
func (c *Client) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, bucket, key, nil, nil, EmptyPayloadHash, 0, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// PutObject uploads a local file. The payload is hashed first so the request is
// fully signed, which keeps the client correct against plain-http endpoints too.
func (c *Client) PutObject(ctx context.Context, bucket, key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := hex.EncodeToString(h.Sum(nil))

	resp, err := c.do(ctx, http.MethodPut, bucket, key, nil, f, hash, fi.Size(),
		contentTypeFor(key))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// BucketExists reports whether the bucket is present and reachable with these
// credentials. A 403 means "exists but not ours", which is still not usable.
func (c *Client) BucketExists(ctx context.Context, bucket string) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, bucket, "", nil, nil, EmptyPayloadHash, 0, "")
	if err != nil {
		var se *Error
		if ok := asError(err, &se); ok && se.NotFound() {
			return false, nil
		}
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

// MakeBucket creates a bucket, which restore needs on a fresh MinIO.
func (c *Client) MakeBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, http.MethodPut, bucket, "", nil, nil, EmptyPayloadHash, 0, "")
	if err != nil {
		var se *Error
		// Idempotent: an existing bucket we already own is a success.
		if ok := asError(err, &se); ok &&
			(se.Code == "BucketAlreadyOwnedByYou" || se.Code == "BucketAlreadyExists") {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func asError(err error, target **Error) bool {
	return errors.As(err, target)
}

// safeRelPath converts an object key into a path that cannot escape root.
// A key like "../../etc/passwd" is hostile input; treat every key as untrusted.
func safeRelPath(key string) (string, error) {
	norm := strings.ReplaceAll(key, `\`, "/")

	// Inspect the raw segments, not the cleaned path: path.Clean would resolve
	// "../../etc/passwd" into "etc/passwd" and hand back something that looks
	// safe while pointing somewhere nobody asked for. A key containing a
	// parent-directory segment is rejected outright.
	for _, seg := range strings.Split(norm, "/") {
		if seg == ".." {
			return "", fmt.Errorf("object key %q contains a parent-directory segment", key)
		}
	}

	clean := strings.TrimPrefix(path.Clean("/"+norm), "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("object key %q has no usable path", key)
	}
	return filepath.FromSlash(clean), nil
}

// MirrorDown copies every object in a bucket into destDir, preserving keys as
// relative paths. Returns the number of objects and total bytes written.
func (c *Client) MirrorDown(ctx context.Context, bucket, destDir string,
	progress func(key string, n, total int)) (files int, bytes int64, err error) {

	objects, err := c.ListObjects(ctx, bucket, "")
	if err != nil {
		return 0, 0, err
	}
	for i, obj := range objects {
		rel, err := safeRelPath(obj.Key)
		if err != nil {
			return files, bytes, err
		}
		target := filepath.Join(destDir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return files, bytes, err
		}
		if progress != nil {
			progress(obj.Key, i+1, len(objects))
		}
		n, err := c.downloadTo(ctx, bucket, obj.Key, target)
		if err != nil {
			return files, bytes, fmt.Errorf("downloading %s: %w", obj.Key, err)
		}
		if obj.Size > 0 && n != obj.Size {
			return files, bytes, fmt.Errorf("%s: expected %d bytes, got %d", obj.Key, obj.Size, n)
		}
		files++
		bytes += n
	}
	return files, bytes, nil
}

func (c *Client) downloadTo(ctx context.Context, bucket, key, target string) (int64, error) {
	rc, err := c.GetObject(ctx, bucket, key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	tmp := target + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, rc)
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, closeErr
	}
	_ = os.Remove(target)
	if err := os.Rename(tmp, target); err != nil {
		return 0, err
	}
	return n, nil
}

// MirrorUp uploads every file under srcDir into the bucket, using the path
// relative to srcDir as the object key. This is the inverse of MirrorDown and is
// what makes a restore land on identical keys.
func (c *Client) MirrorUp(ctx context.Context, srcDir, bucket string,
	progress func(key string, n, total int)) (files int, bytes int64, err error) {

	var list []string
	err = filepath.Walk(srcDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}
		list = append(list, p)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	for i, p := range list {
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return files, bytes, err
		}
		key := filepath.ToSlash(rel)
		if progress != nil {
			progress(key, i+1, len(list))
		}
		if err := c.PutObject(ctx, bucket, key, p); err != nil {
			return files, bytes, fmt.Errorf("uploading %s: %w", key, err)
		}
		if fi, err := os.Stat(p); err == nil {
			bytes += fi.Size()
		}
		files++
	}
	return files, bytes, nil
}

// contentTypeFor guesses a media type from the extension so restored
// attachments are served correctly rather than as octet-stream.
func contentTypeFor(key string) string {
	switch strings.ToLower(filepath.Ext(key)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	case ".txt", ".md", ".markdown":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".mp4":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

// FormatBytes renders a byte count for humans.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
