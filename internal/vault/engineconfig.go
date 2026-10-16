package vault

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/vault/api"
)

// SecretMountUUIDs returns the UUID of every secret mount, keyed by slash-trimmed path.
func (c *Client) SecretMountUUIDs(ctx context.Context) (map[string]string, error) {
	mounts, err := c.api.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: listing secret mounts: %w", err)
	}
	out := make(map[string]string, len(mounts))
	for p, m := range mounts {
		out[normPath(p)] = m.UUID
	}
	return out, nil
}

// WriteEngineConfig writes body to path (already "<mount>/<path>", no leading
// slash). A plugin's 4xx response can echo the submitted values, and reconcile
// errors are logged verbatim, so an API error carries only the path and status
// code and wraps nothing that could unwrap back to the response body.
func (c *Client) WriteEngineConfig(ctx context.Context, path string, body map[string]any) error {
	_, err := c.api.Logical().WriteWithContext(ctx, normPath(path), body)
	if err == nil {
		return nil
	}
	var respErr *api.ResponseError
	if errors.As(err, &respErr) {
		return fmt.Errorf("vault: writing engine config %s: HTTP %d (response body withheld; it may echo secret values)", path, respErr.StatusCode)
	}
	return fmt.Errorf("vault: writing engine config %s: %w", path, err)
}
