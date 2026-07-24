package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

var ErrAlreadyExists = errors.New("object already exists")

// DirectTransfer follows no redirects and never has access to a control-plane
// credential. It accepts only HTTPS grants for the configured Vultr host and
// exact path-style bucket/key association.
type DirectTransfer struct {
	storageHost string
	bucket      string
	client      *http.Client
	now         func() time.Time
}

func NewDirectTransfer(endpoint, bucket string, client *http.Client) (*DirectTransfer, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("object storage endpoint must be an HTTPS origin")
	}
	if bucket == "" || strings.Contains(bucket, "/") {
		return nil, errors.New("object storage bucket is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &DirectTransfer{
		storageHost: strings.ToLower(parsed.Host), bucket: bucket, client: &clone, now: time.Now,
	}, nil
}

func (t *DirectTransfer) Download(ctx context.Context, grant controlplane.WorkspaceGrant, key string, limit int64, destination io.Writer) (TransferResult, error) {
	if err := t.validateGrant(grant, key, http.MethodGet); err != nil {
		return TransferResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, grant.URL, nil)
	if err != nil {
		return TransferResult{}, fmt.Errorf("create direct GET request: %w", err)
	}
	response, err := t.client.Do(request)
	if err != nil {
		return TransferResult{}, fmt.Errorf("send direct GET request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return TransferResult{}, fmt.Errorf("direct GET returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return TransferResult{}, errors.New("object exceeds configured transfer limit")
	}
	return copyAndHash(destination, response.Body, limit)
}

func (t *DirectTransfer) Upload(ctx context.Context, grant controlplane.WorkspaceGrant, key string, source io.Reader, size int64, expectedSHA256 string) (TransferResult, error) {
	if size < 0 || !validSHA256(expectedSHA256) {
		return TransferResult{}, errors.New("invalid direct upload descriptor")
	}
	if err := t.validateGrant(grant, key, http.MethodPut); err != nil {
		return TransferResult{}, err
	}
	reader := &hashingReader{source: io.LimitReader(source, size+1), hash: sha256.New(), limit: size}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, grant.URL, reader)
	if err != nil {
		return TransferResult{}, fmt.Errorf("create direct PUT request: %w", err)
	}
	request.ContentLength = size
	for name, value := range grant.Headers {
		request.Header.Set(name, value)
	}
	response, err := t.client.Do(request)
	if err != nil {
		return TransferResult{}, fmt.Errorf("send direct PUT request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		return TransferResult{}, ErrAlreadyExists
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return TransferResult{}, fmt.Errorf("direct PUT returned HTTP %d", response.StatusCode)
	}
	if reader.size != size || reader.exceeded || hex.EncodeToString(reader.hash.Sum(nil)) != expectedSHA256 {
		return TransferResult{}, errors.New("direct upload source does not match descriptor")
	}
	return TransferResult{Size: reader.size, SHA256: expectedSHA256}, nil
}

func (t *DirectTransfer) validateGrant(grant controlplane.WorkspaceGrant, key, method string) error {
	if grant.Key != key || grant.Method != method || grant.URL == "" {
		return errors.New("grant does not match expected direct object operation")
	}
	if expiresAt, err := time.Parse(time.RFC3339, grant.ExpiresAt); err != nil || !expiresAt.After(t.now()) {
		return errors.New("direct object grant is expired or invalid")
	}
	parsed, err := url.Parse(grant.URL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil ||
		!strings.EqualFold(parsed.Host, t.storageHost) {
		return errors.New("direct object grant has a foreign storage URL")
	}
	expectedPath := "/" + t.bucket + "/" + key
	if parsed.Path != expectedPath {
		return errors.New("direct object grant does not match expected bucket key")
	}
	if method == http.MethodGet {
		if len(grant.Headers) != 0 {
			return errors.New("direct GET grant has unexpected headers")
		}
		return nil
	}
	if len(grant.Headers) != 1 || grant.Headers["if-none-match"] != "*" {
		return errors.New("direct PUT grant is missing immutable write precondition")
	}
	return nil
}

func copyAndHash(destination io.Writer, source io.Reader, limit int64) (TransferResult, error) {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, limit+1))
	if err != nil {
		return TransferResult{}, err
	}
	if written > limit {
		return TransferResult{}, errors.New("object exceeds configured transfer limit")
	}
	return TransferResult{Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

type hashingReader struct {
	source   io.Reader
	hash     hash.Hash
	size     int64
	limit    int64
	exceeded bool
}

func (r *hashingReader) Read(buffer []byte) (int, error) {
	count, err := r.source.Read(buffer)
	if count > 0 {
		r.size += int64(count)
		if r.size > r.limit {
			r.exceeded = true
			return 0, errors.New("upload source exceeds descriptor size")
		}
		if _, hashErr := r.hash.Write(buffer[:count]); hashErr != nil {
			return 0, hashErr
		}
	}
	return count, err
}
