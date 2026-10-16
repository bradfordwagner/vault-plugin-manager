package k8s

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrNotFound is wrapped by GetSecretData when the Secret does not exist.
var ErrNotFound = errors.New("not found")

// GetSecretData returns the Secret's data and resourceVersion. A missing Secret
// returns an error wrapping ErrNotFound; every other failure is returned as is.
func (c *Client) GetSecretData(ctx context.Context, ns, name string) (map[string][]byte, string, error) {
	s, err := c.clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("k8s: secret %s/%s: %w", ns, name, ErrNotFound)
		}
		return nil, "", err
	}
	return s.Data, s.ResourceVersion, nil
}

// PutSecretData creates the Secret when resourceVersion is "", otherwise updates
// it guarded by resourceVersion. Returns the new resourceVersion.
func (c *Client) PutSecretData(ctx context.Context, ns, name, resourceVersion string, data map[string][]byte) (string, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ns,
			ResourceVersion: resourceVersion,
			Labels:          map[string]string{"app.kubernetes.io/managed-by": "vault-plugin-manager"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	secrets := c.clientset.CoreV1().Secrets(ns)
	var (
		out *corev1.Secret
		err error
	)
	if resourceVersion == "" {
		out, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
	} else {
		out, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		return "", err
	}
	return out.ResourceVersion, nil
}
