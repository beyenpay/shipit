package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckRepoAndTokenStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.Header().Set("github-authentication-token-expiration", "2027-03-01 00:00:00 UTC")
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	mk := func(tok string) *Client { return &Client{BaseURL: srv.URL, Token: tok, HTTP: srv.Client()} }

	if err := mk("good").CheckRepo(ctx, "o/r"); err != nil {
		t.Error(err)
	}
	if err := mk("bad").CheckRepo(ctx, "o/r"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("bad token: %v", err)
	}
	if err := mk("good").CheckRepo(ctx, "nope"); err == nil {
		t.Error("invalid repo accepted")
	}

	exp, err := mk("good").TokenStatus(ctx)
	if err != nil || exp != "2027-03-01 00:00:00 UTC" {
		t.Errorf("TokenStatus = %q, %v", exp, err)
	}
	if _, err := mk("bad").TokenStatus(ctx); err == nil || !strings.Contains(err.Error(), "token invalid or expired") {
		t.Errorf("bad token status: %v", err)
	}
}
