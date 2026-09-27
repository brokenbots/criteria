package oci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
)

// TestPullerNewRepository_DedicatedClient pins the leak-fix contract in
// newRepository: every pull builds its own http.Client with a dedicated
// hardened transport (never the process-global DefaultClient/DefaultTransport
// whose pooled connections park idle HTTP/2 readers forever), and the
// returned cleanup closes exactly that client's idle connections, so
// completed pulls leak nothing.
func TestPullerNewRepository_DedicatedClientAndCleanup(t *testing.T) {
	p := &Puller{}
	repo, closeIdle, err := p.newRepository(Reference{Registry: "127.0.0.1:1", Repo: "example/adapter"})
	if err != nil {
		t.Fatalf("newRepository: %v", err)
	}
	defer closeIdle()

	if repo.Client == nil {
		t.Fatal("repo.Client is nil, want a dedicated auth client")
	}
	authC, ok := repo.Client.(*auth.Client)
	if !ok {
		t.Fatalf("repo.Client is %T, want *auth.Client", repo.Client)
	}
	if authC.Client == nil || authC.Client == http.DefaultClient || authC.Client == auth.DefaultClient.Client {
		t.Error("newRepository wired a shared http client; a completed pull would leak its pooled connections")
	}
	transport, ok := authC.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", authC.Client.Transport)
	}
	if !transport.ForceAttemptHTTP2 || transport.TLSHandshakeTimeout == 0 || transport.IdleConnTimeout == 0 {
		t.Errorf("client transport = %+v, want the hardened pull transport", transport)
	}
	if !repo.PlainHTTP {
		t.Error("repo.PlainHTTP = false for a 127.0.0.1 registry, want IsLocalhost to enable plain HTTP")
	}

	// The cleanup must be safe to call more than once (callers defer it on
	// every code path that may return).
	closeIdle()
	closeIdle()
}

func TestPullerNewRepository_AuthAndPlainHTTPSettings(t *testing.T) {
	p := &Puller{PlainHTTP: true, Auth: DefaultAuthProvider()}
	repo, closeIdle, err := p.newRepository(Reference{Registry: "ghcr.io", Repo: "org/adapter"})
	if err != nil {
		t.Fatalf("newRepository: %v", err)
	}
	defer closeIdle()
	if !repo.PlainHTTP {
		t.Error("repo.PlainHTTP = false with Puller.PlainHTTP set, want it honored")
	}
	if repo.Client == nil {
		t.Error("repo.Client must still be a dedicated client")
	} else if authC, ok := repo.Client.(*auth.Client); ok && (authC.Client == http.DefaultClient || authC.Client == auth.DefaultClient.Client) {
		t.Error("repo.Client must not share the process-global http client")
	}
}

func TestPullerNewRepository_RejectsInvalidReference(t *testing.T) {
	p := &Puller{}
	_, _, err := p.newRepository(Reference{Registry: "!! not a registry !!", Repo: "x"})
	if err == nil {
		t.Error("newRepository with an invalid reference = nil error, want build failure")
	}
}

// TestPullerListTags_FetchesFromRegistry drives ListTags end-to-end against a
// stub OCI registry, covering the caller wiring (newRepository + deferred
// cleanup + tag paging).
func TestPullerListTags_FetchesFromRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/example/adapter/tags/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"example/adapter","tags":["v1","v2"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := &Puller{PlainHTTP: true}
	tags, err := p.ListTags(context.Background(), Reference{Registry: srv.Listener.Addr().String(), Repo: "example/adapter"})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 || tags[0] != "v1" || tags[1] != "v2" {
		t.Errorf("ListTags = %v, want [v1 v2]", tags)
	}
}

func TestPullerListTags_RequiresRegistryAndRepo(t *testing.T) {
	p := &Puller{}
	if _, err := p.ListTags(context.Background(), Reference{Registry: "", Repo: "x"}); err == nil {
		t.Error("ListTags without a registry = nil error, want error")
	}
}
