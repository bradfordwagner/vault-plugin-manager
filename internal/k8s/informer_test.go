package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWatchConfigMapDeliversAndExposesTheInformer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-plugins", Namespace: "vault"},
		Data:       map[string]string{"plugins.yaml": "settings: {}"},
	}
	c := &Client{clientset: fake.NewClientset(cm)}

	got := make(chan string, 4)
	informer, err := c.WatchConfigMap(ctx, "vault", "vault-plugins", 0, ConfigMapHandler{
		OnChange: func(cm *corev1.ConfigMap) { got <- cm.Data["plugins.yaml"] },
	})
	if err != nil {
		t.Fatalf("WatchConfigMap: %v", err)
	}

	select {
	case data := <-got:
		if data != "settings: {}" {
			t.Fatalf("delivered %q", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no OnChange within 5s")
	}

	// The informer handle is what the liveness probe interrogates.
	if informer.IsStopped() {
		t.Fatal("want a running informer")
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for !informer.IsStopped() {
		if time.Now().After(deadline) {
			t.Fatal("informer did not stop after the context was cancelled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Watch churn must not be reported as a failure: the apiserver closes watches on
// a timer and ages out resource versions, and client-go relists on both.
func TestBenignWatchError(t *testing.T) {
	gone := apierrors.NewGone("too old")
	expired := apierrors.NewResourceExpired("resource version too old")
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "vault-plugins")

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"eof", io.EOF, true},
		{"wrapped eof", fmt.Errorf("watch: %w", io.EOF), true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"context cancelled", context.Canceled, true},
		{"gone", gone, true},
		{"resource expired", expired, true},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "vault-plugins", errors.New("rbac")), false},
		{"not found", notFound, false},
		{"connection refused", errors.New("dial tcp 10.0.0.1:443: connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := benignWatchError(tc.err); got != tc.want {
				t.Fatalf("benignWatchError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
