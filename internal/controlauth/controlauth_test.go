package controlauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const webSA = "system:serviceaccount:zumble-zay:zumble-zay"

type fakeReviewer struct {
	status       authnv1.TokenReviewStatus
	err          error
	gotToken     string
	gotAudiences []string
}

func (f *fakeReviewer) Create(_ context.Context, tr *authnv1.TokenReview, _ metav1.CreateOptions) (*authnv1.TokenReview, error) {
	f.gotToken = tr.Spec.Token
	f.gotAudiences = tr.Spec.Audiences
	if f.err != nil {
		return nil, f.err
	}
	return &authnv1.TokenReview{Status: f.status}, nil
}

func withBearer(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/control/redeem", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestAcceptsAllowedServiceAccount(t *testing.T) {
	f := &fakeReviewer{status: authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"zumble-zay-orchestrator"},
		User:          authnv1.UserInfo{Username: webSA},
	}}
	a := New(f, "zumble-zay-orchestrator", []string{webSA}, nil)
	caller, err := a.Authenticate(withBearer("projected-token"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if caller.Subject != webSA || !caller.Trusted {
		t.Errorf("caller = %+v, want the web SA and Trusted", caller)
	}
	if f.gotToken != "projected-token" {
		t.Errorf("reviewed token = %q, want projected-token", f.gotToken)
	}
	if len(f.gotAudiences) != 1 || f.gotAudiences[0] != "zumble-zay-orchestrator" {
		t.Errorf("reviewed audiences = %v, want [zumble-zay-orchestrator]", f.gotAudiences)
	}
}

func TestRejectsUnauthenticated(t *testing.T) {
	f := &fakeReviewer{status: authnv1.TokenReviewStatus{Authenticated: false, Error: "bad token"}}
	a := New(f, "aud", []string{webSA}, nil)
	if _, err := a.Authenticate(withBearer("t")); err == nil {
		t.Fatal("want error for an unauthenticated token")
	}
}

func TestRejectsWrongAudience(t *testing.T) {
	f := &fakeReviewer{status: authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"some-other-audience"},
		User:          authnv1.UserInfo{Username: webSA},
	}}
	a := New(f, "zumble-zay-orchestrator", []string{webSA}, nil)
	if _, err := a.Authenticate(withBearer("t")); err == nil {
		t.Fatal("want error when the token audience excludes ours")
	}
}

func TestRejectsDisallowedSubject(t *testing.T) {
	f := &fakeReviewer{status: authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"aud"},
		User:          authnv1.UserInfo{Username: "system:serviceaccount:kube-system:someone-else"},
	}}
	a := New(f, "aud", []string{webSA}, nil)
	if _, err := a.Authenticate(withBearer("t")); err == nil {
		t.Fatal("want error for a ServiceAccount not on the allowlist")
	}
}

func TestRejectsNoBearer(t *testing.T) {
	f := &fakeReviewer{}
	a := New(f, "aud", []string{webSA}, nil)
	if _, err := a.Authenticate(withBearer("")); err == nil {
		t.Fatal("want error when no bearer token is presented")
	}
	if f.gotToken != "" {
		t.Error("must not call TokenReview when there is no token")
	}
}

func TestReviewErrorSurfaces(t *testing.T) {
	f := &fakeReviewer{err: errors.New("apiserver down")}
	a := New(f, "aud", []string{webSA}, nil)
	if _, err := a.Authenticate(withBearer("t")); err == nil {
		t.Fatal("want error when TokenReview itself fails")
	}
}
