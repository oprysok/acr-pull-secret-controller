package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func fakeJWT(exp time.Time) string {
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

type fakeCred struct{ calls int }

func (f *fakeCred) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.calls++
	if len(opts.Scopes) != 1 || opts.Scopes[0] != acrScope {
		return azcore.AccessToken{}, fmt.Errorf("unexpected scopes %v", opts.Scopes)
	}
	return azcore.AccessToken{Token: "entra-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// registry returns a TLS test server acting as an ACR /oauth2/exchange endpoint.
func registry(t *testing.T, refreshToken string, exchanges *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/exchange" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		want := url.Values{"grant_type": {"access_token"}, "tenant": {"tenant-b"}, "access_token": {"entra-token"}}
		for k, v := range want {
			if r.PostForm.Get(k) != v[0] {
				http.Error(w, "bad "+k, http.StatusBadRequest)
				return
			}
		}
		*exchanges++
		_ = json.NewEncoder(w).Encode(map[string]string{"refresh_token": refreshToken})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExpiry(t *testing.T) {
	exp := time.Unix(1_900_000_000, 0)
	fallback := time.Unix(1, 0)
	if got := expiry(fakeJWT(exp), fallback); !got.Equal(exp) {
		t.Fatalf("expiry = %v, want %v", got, exp)
	}
	if got := expiry("not-a-jwt", fallback); !got.Equal(fallback) {
		t.Fatalf("expiry of garbage = %v, want fallback", got)
	}
}

func TestProviderCachesUntilRenewal(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := start
	exchanges := 0
	srv := registry(t, fakeJWT(start.Add(3*time.Hour)), &exchanges)
	host := srv.Listener.Addr().String()

	cred := &fakeCred{}
	p := NewProvider(cred, "tenant-b", []string{host}, 0.5,
		WithHTTPClient(srv.Client()), WithClock(func() time.Time { return now }))

	creds, renewAt, err := p.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Registry != host {
		t.Fatalf("unexpected credentials %+v", creds)
	}
	if want := start.Add(90 * time.Minute); !renewAt.Equal(want) {
		t.Fatalf("renewAt = %v, want %v", renewAt, want)
	}

	now = start.Add(89 * time.Minute)
	if _, _, err := p.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cred.calls != 1 || exchanges != 1 {
		t.Fatalf("expected cached result, got %d Entra calls and %d exchanges", cred.calls, exchanges)
	}

	now = start.Add(91 * time.Minute)
	if _, _, err := p.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cred.calls != 2 || exchanges != 2 {
		t.Fatalf("expected renewal, got %d Entra calls and %d exchanges", cred.calls, exchanges)
	}
}

func TestExchangeError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := exchange(context.Background(), srv.Client(), srv.Listener.Addr().String(), "t", "a")
	if err == nil {
		t.Fatal("expected error")
	}
}
