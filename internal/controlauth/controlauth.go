// Package controlauth authenticates a control-plane caller by its Kubernetes
// workload identity: it validates the caller's projected ServiceAccount token via
// the TokenReview API and checks the token's audience and the caller's
// ServiceAccount against an allowlist (docs/adr/0031). It implements
// controlplane.CallerAuthenticator.
//
// It lives outside internal/controlplane on purpose: the client-go dependency it
// needs must stay off the web tier's import graph (the internet-facing web tier
// holds no Kubernetes client, docs/adr/0023). Only the orchestrator binary imports
// this package.
package controlauth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/jackfrancis/zumble-zay/internal/controlplane"
)

// reviewer creates TokenReviews. The typed clientset's
// AuthenticationV1().TokenReviews() satisfies it; the narrow interface lets tests
// inject a fake without standing up a cluster.
type reviewer interface {
	Create(ctx context.Context, tr *authnv1.TokenReview, opts metav1.CreateOptions) (*authnv1.TokenReview, error)
}

// Authenticator validates a caller's projected ServiceAccount token via TokenReview.
type Authenticator struct {
	reviews  reviewer
	audience string
	allowed  map[string]bool
	log      *slog.Logger
}

var _ controlplane.CallerAuthenticator = (*Authenticator)(nil)

// New builds an Authenticator over a TokenReview reviewer. audience is the value
// the caller's token must be minted for; allowedSubjects is the set of caller
// ServiceAccount usernames permitted (e.g.
// "system:serviceaccount:zumble-zay:zumble-zay").
func New(reviews reviewer, audience string, allowedSubjects []string, log *slog.Logger) *Authenticator {
	allowed := make(map[string]bool, len(allowedSubjects))
	for _, s := range allowedSubjects {
		if s = strings.TrimSpace(s); s != "" {
			allowed[s] = true
		}
	}
	return &Authenticator{reviews: reviews, audience: audience, allowed: allowed, log: log}
}

// Build constructs an Authenticator from the pod's in-cluster ServiceAccount,
// creating the Kubernetes client it uses to call TokenReview. It only works inside
// a cluster (the orchestrator always runs in one).
func Build(audience string, allowedSubjects []string, log *slog.Logger) (*Authenticator, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("controlauth: in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("controlauth: kubernetes client: %w", err)
	}
	return New(cs.AuthenticationV1().TokenReviews(), audience, allowedSubjects, log), nil
}

// Authenticate validates the request's bearer as a projected ServiceAccount token
// scoped to the configured audience and issued to an allowed ServiceAccount. It
// fails closed: any doubt (no token, review error, not authenticated, wrong
// audience, disallowed subject) is an error, so the caller is rejected.
func (a *Authenticator) Authenticate(r *http.Request) (controlplane.Caller, error) {
	tok := bearer(r)
	if tok == "" {
		return controlplane.Caller{}, fmt.Errorf("controlauth: no bearer token")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	review, err := a.reviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: tok, Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return controlplane.Caller{}, fmt.Errorf("controlauth: token review: %w", err)
	}
	if !review.Status.Authenticated {
		return controlplane.Caller{}, fmt.Errorf("controlauth: token not authenticated: %s", review.Status.Error)
	}
	// The token must be scoped to our audience, so a token minted for some other
	// service cannot be replayed against the control API.
	if a.audience != "" && !contains(review.Status.Audiences, a.audience) {
		return controlplane.Caller{}, fmt.Errorf("controlauth: token audience %v excludes %q", review.Status.Audiences, a.audience)
	}
	user := review.Status.User.Username
	if !a.allowed[user] {
		return controlplane.Caller{}, fmt.Errorf("controlauth: caller %q is not an allowed control-plane identity", user)
	}
	return controlplane.Caller{Subject: user, Trusted: true}, nil
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
