package vault

import (
	"context"
	"encoding/json"
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
// slash). A plugin's error response can echo the submitted values, and
// reconcile errors are logged verbatim, so the response body is never read:
// the HTTP status alone decides the outcome, and an error carries only the path
// and status. The raw write matters for correctness too: the secret-parsing
// write reports a 404 whose body is only whitespace as success, which would
// record a write Vault never accepted.
func (c *Client) WriteEngineConfig(ctx context.Context, path string, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("vault: encoding engine config %s: %w", path, err)
	}
	resp, err := c.api.Logical().WriteRawWithContext(ctx, normPath(path), data)
	if resp != nil {
		defer resp.Body.Close()
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	var respErr *api.ResponseError
	if errors.As(err, &respErr) {
		status = respErr.StatusCode
	}
	switch {
	case status != 0 && (status < 200 || status > 299):
		return fmt.Errorf("vault: writing engine config %s: HTTP %d (response body withheld; it may echo secret values)", path, status)
	case err != nil:
		return fmt.Errorf("vault: writing engine config %s: %w", path, err)
	case resp == nil:
		return fmt.Errorf("vault: writing engine config %s: no response", path)
	}
	return nil
}
