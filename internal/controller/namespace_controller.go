// Package controller keeps an image pull secret in every selected namespace.
package controller

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/oprysok/acr-pull-secret-controller/internal/token"
)

const (
	// ManagedByLabel and ManagedByValue mark secrets owned by this controller.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "acr-pull-secret-controller"

	RefreshedAtAnnotation = "acr-pull-secret-controller/refreshed-at"
	ExpiresAtAnnotation   = "acr-pull-secret-controller/expires-at"

	minRequeue = 30 * time.Second
	maxJitter  = 15 * time.Second
)

// CredentialSource supplies registry credentials and when to renew them.
type CredentialSource interface {
	Credentials(ctx context.Context) ([]token.Credential, time.Time, error)
}

// NamespaceReconciler writes the pull secret into namespaces matching Selector
// and removes it from namespaces that stop matching.
type NamespaceReconciler struct {
	client.Client
	Credentials CredentialSource
	Selector    labels.Selector
	SecretName  string
	Recorder    record.EventRecorder
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !r.Selector.Matches(labels.Set(ns.Labels)) {
		return ctrl.Result{}, r.removeSecret(ctx, ns.Name)
	}

	creds, renewAt, err := r.Credentials.Credentials(ctx)
	if err != nil {
		syncTotal.WithLabelValues("token_error").Inc()
		r.event(&ns, corev1.EventTypeWarning, "TokenRefreshFailed", err.Error())
		// Existing secrets stay untouched and keep working until their token expires.
		return ctrl.Result{}, err
	}

	changed, err := r.ensureSecret(ctx, ns.Name, creds)
	if err != nil {
		syncTotal.WithLabelValues("error").Inc()
		r.event(&ns, corev1.EventTypeWarning, "PullSecretSyncFailed", err.Error())
		return ctrl.Result{}, err
	}
	if changed {
		syncTotal.WithLabelValues("updated").Inc()
		log.Info("pull secret written", "namespace", ns.Name, "secret", r.SecretName)
	} else {
		syncTotal.WithLabelValues("unchanged").Inc()
	}

	return ctrl.Result{RequeueAfter: requeueAfter(time.Until(renewAt))}, nil
}

// ensureSecret creates or updates the managed secret. It refuses to touch a
// same-named secret it doesn't manage.
func (r *NamespaceReconciler) ensureSecret(ctx context.Context, namespace string, creds []token.Credential) (bool, error) {
	data, err := DockerConfigJSON(creds)
	if err != nil {
		return false, err
	}
	annotations := annotationsFor(creds)

	var existing corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: r.SecretName}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        r.SecretName,
				Namespace:   namespace,
				Labels:      map[string]string{ManagedByLabel: ManagedByValue},
				Annotations: annotations,
			},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: data},
		}
		if err := r.Create(ctx, secret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, unmanagedError(namespace, r.SecretName)
			}
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	}

	if existing.Labels[ManagedByLabel] != ManagedByValue {
		return false, unmanagedError(namespace, r.SecretName)
	}
	if bytes.Equal(existing.Data[corev1.DockerConfigJsonKey], data) {
		return false, nil
	}
	existing.Data = map[string][]byte{corev1.DockerConfigJsonKey: data}
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		existing.Annotations[k] = v
	}
	return true, r.Update(ctx, &existing)
}

func (r *NamespaceReconciler) removeSecret(ctx context.Context, namespace string) error {
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: r.SecretName}, &existing)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if existing.Labels[ManagedByLabel] != ManagedByValue {
		return nil
	}
	if err := r.Delete(ctx, &existing); client.IgnoreNotFound(err) != nil {
		return err
	}
	syncTotal.WithLabelValues("removed").Inc()
	ctrl.LoggerFrom(ctx).Info("pull secret removed", "namespace", namespace, "secret", r.SecretName)
	return nil
}

func (r *NamespaceReconciler) event(obj *corev1.Namespace, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, msg)
	}
}

// SetupWithManager reconciles on namespace changes and on any change to a
// managed secret, so deleted or edited secrets are restored.
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("pull-secret").
		For(&corev1.Namespace{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			if o.GetName() != r.SecretName {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: o.GetNamespace()}}}
		})).
		Complete(r)
}

func annotationsFor(creds []token.Credential) map[string]string {
	var fetched, expires time.Time
	for i, c := range creds {
		if i == 0 || c.FetchedAt.Before(fetched) {
			fetched = c.FetchedAt
		}
		if i == 0 || c.ExpiresOn.Before(expires) {
			expires = c.ExpiresOn
		}
	}
	return map[string]string{
		RefreshedAtAnnotation: fetched.UTC().Format(time.RFC3339),
		ExpiresAtAnnotation:   expires.UTC().Format(time.RFC3339),
	}
}

// requeueAfter spreads namespaces slightly so they don't all reconcile in the same instant.
func requeueAfter(d time.Duration) time.Duration {
	if d < minRequeue {
		d = minRequeue
	}
	return d + rand.N(maxJitter)
}

func unmanagedError(namespace, name string) error {
	return fmt.Errorf("secret %s/%s exists but is not managed by this controller (missing label %s=%s); delete it to let the controller take over",
		namespace, name, ManagedByLabel, ManagedByValue)
}
