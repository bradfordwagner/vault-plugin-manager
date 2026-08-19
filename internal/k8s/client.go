// Package k8s provides the Kubernetes-side plumbing the manager needs: an
// in-cluster client, Vault pod discovery, exec-copy of plugin binaries into the
// Vault containers, and a ConfigMap informer.
package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps a Kubernetes clientset plus the rest config needed for exec.
type Client struct {
	clientset kubernetes.Interface
	rest      *rest.Config
}

// New builds a Client, preferring in-cluster config and falling back to the
// default kubeconfig loading rules for out-of-cluster (local) use.
func New() (*Client, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: loading config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: creating clientset: %w", err)
	}
	return &Client{clientset: cs, rest: cfg}, nil
}

// Clientset exposes the underlying clientset (used by the informer).
func (c *Client) Clientset() kubernetes.Interface { return c.clientset }

func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	return cc.ClientConfig()
}
