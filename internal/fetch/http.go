package fetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

func fetchHTTP(ctx context.Context, req Request) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch: building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch: GET %s: %w", req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch: GET %s: status %s", req.URL, resp.Status)
	}
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch: reading %s: %w", req.URL, err)
	}
	return extractBinary(req.URL, req.Binary, blob)
}

// extractBinary pulls the plugin binary out of a downloaded blob, dispatching on
// the URL suffix: zip, tar, tar.gz/tgz, gz, or a raw binary.
func extractBinary(url, binary string, blob []byte) ([]byte, error) {
	lower := strings.ToLower(url)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(blob, binary)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		gz, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			return nil, fmt.Errorf("fetch: gzip: %w", err)
		}
		defer gz.Close()
		return extractTar(gz, binary)
	case strings.HasSuffix(lower, ".tar"):
		return extractTar(bytes.NewReader(blob), binary)
	case strings.HasSuffix(lower, ".gz"):
		gz, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			return nil, fmt.Errorf("fetch: gzip: %w", err)
		}
		defer gz.Close()
		b, err := io.ReadAll(gz)
		if err != nil {
			return nil, fmt.Errorf("fetch: gunzip: %w", err)
		}
		return b, nil
	default:
		return blob, nil // raw binary
	}
}

func extractZip(blob []byte, binary string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return nil, fmt.Errorf("fetch: opening zip: %w", err)
	}
	files := make(map[string]*zip.File)
	var names []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		files[f.Name] = f
		names = append(names, f.Name)
	}
	chosen, err := selectEntry(names, binary)
	if err != nil {
		return nil, fmt.Errorf("fetch: zip: %w", err)
	}
	rc, err := files[chosen].Open()
	if err != nil {
		return nil, fmt.Errorf("fetch: opening %s in zip: %w", chosen, err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func extractTar(r io.Reader, binary string) ([]byte, error) {
	tr := tar.NewReader(r)
	contents := make(map[string][]byte)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("fetch: reading tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("fetch: reading %s in tar: %w", hdr.Name, err)
		}
		contents[hdr.Name] = b
		names = append(names, hdr.Name)
	}
	chosen, err := selectEntry(names, binary)
	if err != nil {
		return nil, fmt.Errorf("fetch: tar: %w", err)
	}
	return contents[chosen], nil
}

// selectEntry chooses which archive entry is the plugin binary. When binary is
// set it must match exactly one entry by basename; otherwise the archive must
// contain exactly one regular file.
func selectEntry(names []string, binary string) (string, error) {
	if binary != "" {
		var matches []string
		for _, n := range names {
			if path.Base(n) == binary {
				matches = append(matches, n)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return "", fmt.Errorf("binary %q not found (entries: %v)", binary, names)
		default:
			return "", fmt.Errorf("binary %q is ambiguous (matches: %v)", binary, matches)
		}
	}
	if len(names) == 1 {
		return names[0], nil
	}
	return "", fmt.Errorf("archive has %d files; set source.binary to select one (entries: %v)", len(names), names)
}
