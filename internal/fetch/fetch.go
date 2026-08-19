// Package fetch retrieves plugin binaries from an HTTPS URL (raw or archived) or
// an OCI image, extracts the binary, and reports its sha256.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Request describes a plugin binary to fetch. Exactly one of URL or Image is set.
type Request struct {
	URL    string // HTTPS source (raw binary or .zip/.tar/.tar.gz/.tgz/.gz archive)
	Image  string // OCI image reference
	Path   string // OCI: path to the binary inside the image rootfs
	Binary string // archive: binary file name inside the archive (basename match)
	SHA256 string // optional expected checksum (hex) of the extracted binary
}

// Result is a fetched, extracted plugin binary and its computed checksum.
type Result struct {
	Binary []byte
	SHA256 string // hex-encoded sha256 of Binary
}

// Fetcher fetches plugin binaries. It is an interface so the reconciler can be
// tested with a fake.
type Fetcher interface {
	Fetch(ctx context.Context, req Request) (*Result, error)
}

// Client is the default Fetcher supporting HTTPS and OCI sources.
type Client struct {
	insecureOCI bool
}

// Option configures a Client.
type Option func(*Client)

// WithInsecureOCI allows pulling OCI images from registries served over plain
// HTTP or with untrusted TLS (e.g. an in-cluster registry). Leave off in
// production unless a trusted private registry requires it.
func WithInsecureOCI(v bool) Option {
	return func(c *Client) { c.insecureOCI = v }
}

// NewClient returns the default fetcher.
func NewClient(opts ...Option) *Client {
	c := &Client{}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Fetch retrieves and extracts the binary, computes its sha256, and verifies it
// against req.SHA256 when that is provided (failing closed on mismatch).
func (c *Client) Fetch(ctx context.Context, req Request) (*Result, error) {
	var (
		bin []byte
		err error
	)
	switch {
	case req.URL != "" && req.Image != "":
		return nil, fmt.Errorf("fetch: request has both url and image")
	case req.URL != "":
		bin, err = fetchHTTP(ctx, req)
	case req.Image != "":
		bin, err = fetchOCI(ctx, req, c.insecureOCI)
	default:
		return nil, fmt.Errorf("fetch: request has neither url nor image")
	}
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(bin)
	hexsum := hex.EncodeToString(sum[:])
	if req.SHA256 != "" && !strings.EqualFold(req.SHA256, hexsum) {
		return nil, fmt.Errorf("fetch: checksum mismatch: got %s want %s", hexsum, req.SHA256)
	}
	return &Result{Binary: bin, SHA256: hexsum}, nil
}
