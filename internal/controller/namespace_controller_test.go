package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oprysok/acr-pull-secret-controller/internal/token"
)

const secretName = "acr-pull"

type staticSource struct {
	creds []token.Credential
	err   error
}

func (s *staticSource) Credentials(context.Context) ([]token.Credential, time.Time, error) {
	return s.creds, time.Now().Add(time.Hour), s.err
}

func newReconciler(t *testing.T, src CredentialSource, objs ...client.Object) (*NamespaceReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	sel, err := labels.Parse("pull-secret=enabled")
	if err != nil {
		t.Fatal(err)
	}
	return &NamespaceReconciler{Client: c, Credentials: src, Selector: sel, SecretName: secretName}, c
}

func namespace(name string, lbls map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}}
}

func creds(rt string) []token.Credential {
	now := time.Now()
	return []token.Credential{{Registry: "example.azurecr.io", RefreshToken: rt, FetchedAt: now, ExpiresOn: now.Add(3 * time.Hour)}}
}

func reconcileNS(t *testing.T, r *NamespaceReconciler, name string) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
}

func getSecret(t *testing.T, c client.Client, ns string) (*corev1.Secret, error) {
	t.Helper()
	var s corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: secretName}, &s)
	return &s, err
}

func TestCreatesSecretInSelectedNamespace(t *testing.T) {
	r, c := newReconciler(t, &staticSource{creds: creds("rt-1")}, namespace("team-a", map[string]string{"pull-secret": "enabled"}))

	res, err := reconcileNS(t, r, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected a requeue, got %+v", res)
	}

	s, err := getSecret(t, c, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != corev1.SecretTypeDockerConfigJson || s.Labels[ManagedByLabel] != ManagedByValue {
		t.Fatalf("unexpected secret metadata: type=%s labels=%v", s.Type, s.Labels)
	}
	var cfg struct {
		Auths map[string]dockerAuth `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatal(err)
	}
	if a := cfg.Auths["example.azurecr.io"]; a.Username != acrTokenUser || a.Password != "rt-1" {
		t.Fatalf("unexpected auth entry %+v", a)
	}
}

func TestUpdatesSecretWhenTokenChanges(t *testing.T) {
	src := &staticSource{creds: creds("rt-1")}
	r, c := newReconciler(t, src, namespace("team-a", map[string]string{"pull-secret": "enabled"}))
	if _, err := reconcileNS(t, r, "team-a"); err != nil {
		t.Fatal(err)
	}

	src.creds = creds("rt-2")
	if _, err := reconcileNS(t, r, "team-a"); err != nil {
		t.Fatal(err)
	}
	s, _ := getSecret(t, c, "team-a")
	if !strings.Contains(string(s.Data[corev1.DockerConfigJsonKey]), "rt-2") {
		t.Fatal("secret was not updated with the new token")
	}
}

func TestSkipsUnselectedNamespace(t *testing.T) {
	r, c := newReconciler(t, &staticSource{creds: creds("rt-1")}, namespace("team-b", nil))
	if _, err := reconcileNS(t, r, "team-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "team-b"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no secret, got err=%v", err)
	}
}

func TestRemovesSecretWhenNamespaceOptsOut(t *testing.T) {
	managed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "team-c", Labels: map[string]string{ManagedByLabel: ManagedByValue}}}
	r, c := newReconciler(t, &staticSource{creds: creds("rt-1")}, namespace("team-c", nil), managed)
	if _, err := reconcileNS(t, r, "team-c"); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "team-c"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected secret to be removed, got err=%v", err)
	}
}

func TestLeavesUnmanagedSecretAlone(t *testing.T) {
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "team-d"}, Data: map[string][]byte{"x": []byte("y")}}
	r, c := newReconciler(t, &staticSource{creds: creds("rt-1")}, namespace("team-d", map[string]string{"pull-secret": "enabled"}), foreign)

	if _, err := reconcileNS(t, r, "team-d"); err == nil {
		t.Fatal("expected an error for an unmanaged secret")
	}
	s, _ := getSecret(t, c, "team-d")
	if string(s.Data["x"]) != "y" {
		t.Fatal("unmanaged secret was modified")
	}
}

func TestTokenErrorKeepsExistingSecret(t *testing.T) {
	src := &staticSource{creds: creds("rt-1")}
	r, c := newReconciler(t, src, namespace("team-e", map[string]string{"pull-secret": "enabled"}))
	if _, err := reconcileNS(t, r, "team-e"); err != nil {
		t.Fatal(err)
	}

	src.creds, src.err = nil, errors.New("entra unavailable")
	if _, err := reconcileNS(t, r, "team-e"); err == nil {
		t.Fatal("expected the token error to be returned for backoff")
	}
	s, err := getSecret(t, c, "team-e")
	if err != nil || !strings.Contains(string(s.Data[corev1.DockerConfigJsonKey]), "rt-1") {
		t.Fatalf("existing secret should be untouched, err=%v", err)
	}
}
