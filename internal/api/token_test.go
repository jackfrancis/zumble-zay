package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRedeemer stands in for the control-plane ticket redemption.
type fakeRedeemer struct {
	tok string
	exp int
	err error
	got string
}

func (f *fakeRedeemer) RedeemTicket(_ context.Context, ticket string) (string, int, error) {
	f.got = ticket
	return f.tok, f.exp, f.err
}

func TestRedeemReturnsTokenForValidTicket(t *testing.T) {
	f := &fakeRedeemer{tok: "job-token", exp: 600}
	h := NewTokenHandler(f, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/token", strings.NewReader(`{"ticket":"t1"}`))
	h.Redeem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.AccessToken != "job-token" || out.TokenType != "Bearer" || out.ExpiresIn != 600 {
		t.Fatalf("unexpected response: %+v", out)
	}
	if f.got != "t1" {
		t.Errorf("redeemer got ticket %q, want t1", f.got)
	}
}

func TestRedeemRejectsMissingTicket(t *testing.T) {
	h := NewTokenHandler(&fakeRedeemer{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/token", strings.NewReader(`{}`))
	h.Redeem(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRedeemRejectsBadTicket(t *testing.T) {
	// An unknown, spent, or expired ticket is a coarse 401 that reveals nothing.
	h := NewTokenHandler(&fakeRedeemer{err: errors.New("unknown or spent ticket")}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/token", strings.NewReader(`{"ticket":"bad"}`))
	h.Redeem(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
