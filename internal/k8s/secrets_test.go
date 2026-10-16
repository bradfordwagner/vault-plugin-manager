package k8s

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var secretsRes = schema.GroupResource{Resource: "secrets"}

func TestGetSecretDataNotFoundWrapsErrNotFound(t *testing.T) {
	c := &Client{clientset: fake.NewClientset()}
	_, _, err := c.GetSecretData(context.Background(), "vault", "state")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetSecretDataForbiddenIsNotErrNotFound(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(secretsRes, "state", errors.New("denied"))
	})
	c := &Client{clientset: cs}
	_, _, err := c.GetSecretData(context.Background(), "vault", "state")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want non-nil and not ErrNotFound", err)
	}
}

func TestGetSecretDataReturnsDataAndResourceVersion(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: "vault", ResourceVersion: "5"},
			Data:       map[string][]byte{"k": []byte("v")},
		}, nil
	})
	c := &Client{clientset: cs}
	data, rv, err := c.GetSecretData(context.Background(), "vault", "state")
	if err != nil {
		t.Fatal(err)
	}
	if rv != "5" || !reflect.DeepEqual(data, map[string][]byte{"k": []byte("v")}) {
		t.Fatalf("got %v rv=%q", data, rv)
	}
}

func TestPutSecretDataCreateSendsNoResourceVersion(t *testing.T) {
	cs := fake.NewClientset()
	var got *corev1.Secret
	cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).DeepCopy()
		got = s.DeepCopy()
		s.ResourceVersion = "7"
		return true, s, nil
	})
	c := &Client{clientset: cs}
	data := map[string][]byte{"engine-configs.json": []byte("{}")}
	rv, err := c.PutSecretData(context.Background(), "vault", "state", "", data)
	if err != nil {
		t.Fatal(err)
	}
	if rv != "7" {
		t.Fatalf("rv = %q, want 7", rv)
	}
	if got.ResourceVersion != "" || got.Type != corev1.SecretTypeOpaque ||
		got.Labels["app.kubernetes.io/managed-by"] != "vault-plugin-manager" ||
		got.Name != "state" || got.Namespace != "vault" || !reflect.DeepEqual(got.Data, data) {
		t.Fatalf("unexpected created secret: %+v", got)
	}
}

func TestPutSecretDataUpdateSendsSuppliedResourceVersion(t *testing.T) {
	cs := fake.NewClientset()
	var got *corev1.Secret
	cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).DeepCopy()
		got = s.DeepCopy()
		s.ResourceVersion = "8"
		return true, s, nil
	})
	c := &Client{clientset: cs}
	data := map[string][]byte{"engine-configs.json": []byte("{}")}
	rv, err := c.PutSecretData(context.Background(), "vault", "state", "7", data)
	if err != nil {
		t.Fatal(err)
	}
	if rv != "8" {
		t.Fatalf("rv = %q, want 8", rv)
	}
	if got.ResourceVersion != "7" || !reflect.DeepEqual(got.Data, data) {
		t.Fatalf("unexpected updated secret: %+v", got)
	}
	if got.Name != "state" || got.Namespace != "vault" {
		t.Errorf("name/namespace = %s/%s, want vault/state", got.Namespace, got.Name)
	}
	if got.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %q, want %q", got.Type, corev1.SecretTypeOpaque)
	}
	if v := got.Labels["app.kubernetes.io/managed-by"]; v != "vault-plugin-manager" {
		t.Errorf("managed-by label = %q, want vault-plugin-manager", v)
	}
}

func TestPutSecretDataUpdateConflictIsReturned(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if s.ResourceVersion != "7" {
			t.Errorf("sent rv = %q, want 7", s.ResourceVersion)
		}
		return true, nil, apierrors.NewConflict(secretsRes, "state", errors.New("stale"))
	})
	c := &Client{clientset: cs}
	_, err := c.PutSecretData(context.Background(), "vault", "state", "7", map[string][]byte{"a": []byte("b")})
	if !apierrors.IsConflict(err) {
		t.Fatalf("err = %v, want conflict", err)
	}
}
