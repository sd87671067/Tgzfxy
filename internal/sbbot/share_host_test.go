package sbbot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestShareHostExplicitPriority(t *testing.T) {
	host, err := resolveShareHost(context.Background(), " example.org ", nil, nil)
	if err != nil || host != "example.org" {
		t.Fatal("explicit host was not preserved")
	}
}
func TestShareHostPublicIPv4Fallback(t *testing.T) {
	for _, invalid := range []string{"10.0.0.1", "127.0.0.1", "::1", "2001:4860:4860::8888", "100.64.0.1", "192.0.2.1", "198.18.0.1", "240.0.0.1", "<html>bad</html>"} {
		t.Run(invalid, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/bad" {
					fmt.Fprint(w, invalid)
				} else {
					fmt.Fprint(w, "8.8.8.8\n")
				}
			}))
			defer server.Close()
			host, err := resolveShareHost(context.Background(), "", server.Client(), []string{server.URL + "/bad", server.URL + "/good"})
			if err != nil || host != "8.8.8.8" || calls != 2 {
				t.Fatal("public IPv4 fallback failed")
			}
		})
	}
}
func TestShareHostFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); fmt.Fprint(w, "8.8.8.8") }))
	defer server.Close()
	if _, err := resolveShareHost(context.Background(), "", server.Client(), []string{server.URL}); err == nil {
		t.Fatal("accepted failed discovery")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveShareHost(ctx, "", server.Client(), []string{server.URL}); err == nil {
		t.Fatal("ignored cancellation")
	}
}
