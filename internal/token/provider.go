// Package token obtains ACR refresh tokens through Microsoft Entra workload
// identity federation and caches them until they are due for renewal.
package token

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	acrScope = "https://containerregistry.azure.net/.default"
	// fallbackLifetime is used when the refresh token's exp claim can't be read.
	fallbackLifetime = 3 * time.Hour
)

// Credential is an ACR refresh token for one registry.
type Credential struct {
	Registry     string
	RefreshToken string
	FetchedAt    time.Time
	ExpiresOn    time.Time
}

// Provider fetches refresh tokens for a fixed set of registries. All
// registries share one Entra access token, so a renewal costs one call to
// Entra plus one exchange per registry, regardless of how many namespaces
// consume the result.
type Provider struct {
	cred            azcore.TokenCredential
	tenantID        string
	registries      []string
	refreshFraction float64
	client          *http.Client
	now             func() time.Time

	mu      sync.Mutex
	current []Credential
}

// Option customises a Provider.
type Option func(*Provider)

// WithHTTPClient sets the client used for the ACR token exchange.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.client = c } }

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(p *Provider) { p.now = now } }

// NewProvider returns a Provider. tenantID is the tenant that owns the
// registries; cred must issue tokens in that tenant. refreshFraction is the
// share of a token's lifetime after which it is renewed (0 < f < 1).
func NewProvider(cred azcore.TokenCredential, tenantID string, registries []string, refreshFraction float64, opts ...Option) *Provider {
	p := &Provider{
		cred:            cred,
		tenantID:        tenantID,
		registries:      registries,
		refreshFraction: refreshFraction,
		client:          &http.Client{Timeout: 30 * time.Second},
		now:             time.Now,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Credentials returns a refresh token per registry and the time at which
// they should be renewed. Cached tokens are returned until that time.
func (p *Provider) Credentials(ctx context.Context) ([]Credential, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	if len(p.current) == len(p.registries) && now.Before(p.renewAt()) {
		return p.snapshot(), p.renewAt(), nil
	}

	fresh, err := p.fetch(ctx, now)
	if err != nil {
		refreshTotal.WithLabelValues("error").Inc()
		return nil, time.Time{}, err
	}
	refreshTotal.WithLabelValues("success").Inc()
	for _, c := range fresh {
		expiryTimestamp.WithLabelValues(c.Registry).Set(float64(c.ExpiresOn.Unix()))
	}
	p.current = fresh
	return p.snapshot(), p.renewAt(), nil
}

func (p *Provider) fetch(ctx context.Context, now time.Time) ([]Credential, error) {
	tok, err := p.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{acrScope}})
	if err != nil {
		return nil, fmt.Errorf("get Entra token: %w", err)
	}
	out := make([]Credential, 0, len(p.registries))
	for _, reg := range p.registries {
		rt, err := exchange(ctx, p.client, reg, p.tenantID, tok.Token)
		if err != nil {
			return nil, err
		}
		out = append(out, Credential{
			Registry:     reg,
			RefreshToken: rt,
			FetchedAt:    now,
			ExpiresOn:    expiry(rt, now.Add(fallbackLifetime)),
		})
	}
	return out, nil
}

// renewAt is the earliest renewal time across all cached credentials.
func (p *Provider) renewAt() time.Time {
	var at time.Time
	for i, c := range p.current {
		t := c.FetchedAt.Add(time.Duration(float64(c.ExpiresOn.Sub(c.FetchedAt)) * p.refreshFraction))
		if i == 0 || t.Before(at) {
			at = t
		}
	}
	return at
}

func (p *Provider) snapshot() []Credential {
	return append([]Credential(nil), p.current...)
}
