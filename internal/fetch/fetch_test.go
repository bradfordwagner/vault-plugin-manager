package fetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func zipArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serve(t *testing.T, path string, blob []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob)
	})
	return httptest.NewServer(mux)
}

func TestFetchRawBinary(t *testing.T) {
	want := []byte("i am a plugin binary")
	srv := serve(t, "/foo", want)
	defer srv.Close()

	res, err := NewClient().Fetch(context.Background(), Request{URL: srv.URL + "/foo"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Binary, want) {
		t.Errorf("binary = %q, want %q", res.Binary, want)
	}
	sum := sha256.Sum256(want)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %s, want %s", res.SHA256, hex.EncodeToString(sum[:]))
	}
}

func TestFetchZipSingleFile(t *testing.T) {
	want := []byte("zipped binary")
	blob := zipArchive(t, map[string][]byte{"vault-plugin-foo": want})
	srv := serve(t, "/foo.zip", blob)
	defer srv.Close()

	res, err := NewClient().Fetch(context.Background(), Request{URL: srv.URL + "/foo.zip"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Binary, want) {
		t.Errorf("binary = %q, want %q", res.Binary, want)
	}
}

func TestFetchTarGzSelectsBinary(t *testing.T) {
	want := []byte("the real one")
	blob := tarGzArchive(t, map[string][]byte{
		"README.md":        []byte("docs"),
		"bin/vault-plugin": want,
	})
	srv := serve(t, "/foo.tar.gz", blob)
	defer srv.Close()

	res, err := NewClient().Fetch(context.Background(), Request{URL: srv.URL + "/foo.tar.gz", Binary: "vault-plugin"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Binary, want) {
		t.Errorf("binary = %q, want %q", res.Binary, want)
	}
}

func TestFetchArchiveAmbiguousWithoutBinary(t *testing.T) {
	blob := zipArchive(t, map[string][]byte{"a": []byte("1"), "b": []byte("2")})
	srv := serve(t, "/foo.zip", blob)
	defer srv.Close()

	if _, err := NewClient().Fetch(context.Background(), Request{URL: srv.URL + "/foo.zip"}); err == nil {
		t.Fatal("expected error for ambiguous archive, got nil")
	}
}

func TestFetchChecksumMismatch(t *testing.T) {
	srv := serve(t, "/foo", []byte("content"))
	defer srv.Close()

	_, err := NewClient().Fetch(context.Background(), Request{URL: srv.URL + "/foo", SHA256: "deadbeef"})
	if err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
}

func TestFetchChecksumMatch(t *testing.T) {
	content := []byte("content")
	sum := sha256.Sum256(content)
	srv := serve(t, "/foo", content)
	defer srv.Close()

	if _, err := NewClient().Fetch(context.Background(), Request{URL: srv.URL + "/foo", SHA256: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchNeitherSource(t *testing.T) {
	if _, err := NewClient().Fetch(context.Background(), Request{}); err == nil {
		t.Fatal("expected error when neither url nor image set")
	}
}

func TestNormTarPath(t *testing.T) {
	for in, want := range map[string]string{
		"/plugin/foo":  "plugin/foo",
		"./plugin/foo": "plugin/foo",
		"plugin/foo":   "plugin/foo",
	} {
		if got := normTarPath(in); got != want {
			t.Errorf("normTarPath(%q) = %q, want %q", in, got, want)
		}
	}
}
