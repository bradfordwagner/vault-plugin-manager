package fetch

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// fetchOCI pulls the image, flattens its filesystem, and returns the file at
// req.Path. Registry auth uses the ambient Docker keychain (config/pull secrets).
func fetchOCI(ctx context.Context, req Request) ([]byte, error) {
	if req.Path == "" {
		return nil, fmt.Errorf("fetch: oci source requires path")
	}
	ref, err := name.ParseReference(req.Image)
	if err != nil {
		return nil, fmt.Errorf("fetch: parsing image %q: %w", req.Image, err)
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, fmt.Errorf("fetch: pulling image %q: %w", req.Image, err)
	}

	rc := mutate.Extract(img)
	defer rc.Close()

	want := normTarPath(req.Path)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("fetch: reading image filesystem: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if normTarPath(hdr.Name) == want {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("fetch: reading %s from image: %w", req.Path, err)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("fetch: path %q not found in image %q", req.Path, req.Image)
}

// normTarPath normalizes a path for comparison against flattened-image tar
// entry names, which may be prefixed with "./" or "/".
func normTarPath(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/")
}
