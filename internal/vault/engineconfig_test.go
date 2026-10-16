package vault

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/vault/api"
)

func engineConfigClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	a, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("building api client: %v", err)
	}
	return &Client{api: a}
}

func TestSecretMountUUIDs(t *testing.T) {
	c := engineConfigClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/mounts" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"github/":{"uuid":"u-1","type":"github"},"kv/":{"uuid":"u-2","type":"kv"}}}`))
	})
	got, err := c.SecretMountUUIDs(context.Background())
	if err != nil {
		t.Fatalf("SecretMountUUIDs: %v", err)
	}
	want := map[string]string{"github": "u-1", "kv": "u-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWriteEngineConfigSendsBody(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	c := engineConfigClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	body := map[string]any{"app_id": 4627856, "prv_key": "pem"}
	if err := c.WriteEngineConfig(context.Background(), "github/config", body); err != nil {
		t.Fatalf("WriteEngineConfig: %v", err)
	}
	if gotPath != "/v1/github/config" {
		t.Errorf("path = %q", gotPath)
	}
	if gotMethod != http.MethodPut && gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	want := map[string]any{"app_id": float64(4627856), "prv_key": "pem"}
	if !reflect.DeepEqual(gotBody, want) {
		t.Errorf("body = %v, want %v", gotBody, want)
	}
}

func TestWriteEngineConfigErrorOmitsResponseBody(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		c := engineConfigClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":["invalid key SECRET-PEM", "...EM"]}`))
		})
		err := c.WriteEngineConfig(context.Background(), "github/config", map[string]any{"prv_key": "SECRET-PEM"})
		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		msg := err.Error()
		for _, want := range []string{"github/config", strconv.Itoa(status)} {
			if !strings.Contains(msg, want) {
				t.Errorf("status %d: error %q missing %q", status, msg, want)
			}
		}
		for _, bad := range []string{"SECRET-PEM", `EM"`, "invalid"} {
			if strings.Contains(msg, bad) {
				t.Errorf("status %d: error %q leaks %q", status, msg, bad)
			}
		}
		var respErr *api.ResponseError
		if errors.As(err, &respErr) {
			t.Errorf("status %d: error unwraps to the response body", status)
		}
	}
}
