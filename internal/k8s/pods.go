package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ListRunningPods returns the names of Running pods matching selector in ns.
// Only Running pods are returned because exec requires a live container.
func (c *Client) ListRunningPods(ctx context.Context, ns, selector string) ([]string, error) {
	pods, err := c.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("k8s: listing pods (ns=%s selector=%q): %w", ns, selector, err)
	}
	var names []string
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			names = append(names, pods.Items[i].Name)
		}
	}
	return names, nil
}

// Exec runs command in pod/container, streaming stdin (when non-nil) and
// returning captured stdout/stderr. A non-zero exit is returned as err.
func (c *Client) Exec(ctx context.Context, ns, pod, container string, command []string, stdin io.Reader) (stdout, stderr string, err error) {
	req := c.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	// SPDY still works across current Kubernetes versions; revisit if a cluster
	// disables it in favor of WebSocket-only streaming.
	executor, err := remotecommand.NewSPDYExecutor(c.rest, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("k8s: creating executor for %s/%s: %w", ns, pod, err)
	}
	var outBuf, errBuf bytes.Buffer
	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	return outBuf.String(), errBuf.String(), streamErr
}

// FileSHA256 returns the hex sha256 of a file in the container, or "" if it does
// not exist. Absence is not an error: the script exits 0 either way.
func (c *Client) FileSHA256(ctx context.Context, ns, pod, container, path string) (string, error) {
	script := fmt.Sprintf(`if [ -f %s ]; then sha256sum %s; fi`, shellQuote(path), shellQuote(path))
	stdout, stderr, err := c.Exec(ctx, ns, pod, container, []string{"sh", "-c", script}, nil)
	if err != nil {
		return "", fmt.Errorf("k8s: sha256 of %s on %s/%s: %w: %s", path, ns, pod, err, strings.TrimSpace(stderr))
	}
	return firstField(stdout), nil
}

// EnsureFile makes path in the container hold content with the given mode. It
// copies via streamed stdin only when the current checksum differs, and
// re-verifies the checksum after writing. It returns whether a copy occurred.
func (c *Client) EnsureFile(ctx context.Context, ns, pod, container, path string, content []byte, sha256hex, mode string) (copied bool, err error) {
	current, err := c.FileSHA256(ctx, ns, pod, container, path)
	if err != nil {
		return false, err
	}
	if current == sha256hex {
		return false, nil
	}

	script := fmt.Sprintf(`cat > %s && chmod %s %s`, shellQuote(path), mode, shellQuote(path))
	if _, stderr, err := c.Exec(ctx, ns, pod, container, []string{"sh", "-c", script}, bytes.NewReader(content)); err != nil {
		return false, fmt.Errorf("k8s: writing %s to %s/%s: %w: %s", path, ns, pod, err, strings.TrimSpace(stderr))
	}

	after, err := c.FileSHA256(ctx, ns, pod, container, path)
	if err != nil {
		return false, err
	}
	if after != sha256hex {
		return false, fmt.Errorf("k8s: checksum mismatch after copy to %s/%s:%s: got %q want %q", ns, pod, path, after, sha256hex)
	}
	return true, nil
}

// RemoveFile deletes a file in the container. A missing file is not an error.
func (c *Client) RemoveFile(ctx context.Context, ns, pod, container, path string) error {
	if _, stderr, err := c.Exec(ctx, ns, pod, container, []string{"sh", "-c", fmt.Sprintf(`rm -f %s`, shellQuote(path))}, nil); err != nil {
		return fmt.Errorf("k8s: removing %s from %s/%s: %w: %s", path, ns, pod, err, strings.TrimSpace(stderr))
	}
	return nil
}

// firstField returns the first whitespace-delimited token (the hash from
// `sha256sum` output), or "" if there is none.
func firstField(s string) string {
	if fields := strings.Fields(s); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// shellQuote single-quotes s for safe interpolation into an sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
