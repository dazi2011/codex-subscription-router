package control

import (
	"net/http/httptest"
	"testing"
)

func TestAuthorizationRequiresHeaderAndRejectsQueryToken(t *testing.T) {
	server := &Server{token: "secret"}
	queryRequest := httptest.NewRequest("GET", "http://127.0.0.1/v1/events?token=secret", nil)
	if server.authorized(queryRequest) {
		t.Fatal("query-string token unexpectedly authorized")
	}
	headerRequest := httptest.NewRequest("GET", "http://127.0.0.1/v1/events", nil)
	headerRequest.Header.Set("X-Codex-Mux-Token", "secret")
	if !server.authorized(headerRequest) {
		t.Fatal("header token was rejected")
	}
}
