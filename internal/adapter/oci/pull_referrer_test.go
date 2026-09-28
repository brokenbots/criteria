package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
)

const (
	stubRepoPath = "example/adapter"
	stubTag      = "v1.0.0"
)

// referrerFailure selects which referrer-copy stage the stub registry breaks.
type referrerFailure int

const (
	// referrerCopySucceeds serves every referrer request normally.
	referrerCopySucceeds referrerFailure = iota
	// referrerDiscoveryFails makes the Referrers API return 500, so
	// copyReferrers fails before any referrer is copied.
	referrerDiscoveryFails
	// referrerBlobCopyFails lets discovery succeed but 404s the signature
	// payload blob, so the per-referrer copy fails mid-copy.
	referrerBlobCopyFails
)

// referrerStubFixture holds the OCI content the stub registry serves: an
// adapter artifact plus a cosign-style signature manifest whose Subject is the
// artifact — exactly the referrer shape the puller copies so signature
// verification can find signatures locally.
type referrerStubFixture struct {
	artifactDesc ocispec.Descriptor
	artifactRaw  []byte
	sigDesc      ocispec.Descriptor
	sigRaw       []byte
	payloadDesc  ocispec.Descriptor
	blobs        map[digest.Digest][]byte
	indexRaw     []byte // Referrers API response: an image index listing sig
}

// addStubBlob registers raw blob content and returns its descriptor.
func addStubBlob(f *referrerStubFixture, data []byte, mediaType string) ocispec.Descriptor {
	d := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	f.blobs[d.Digest] = data
	return d
}

// addStubManifest marshals m as OCI content, registers it as a blob too (so
// either endpoint can serve it), and returns its descriptor.
func addStubManifest(t *testing.T, f *referrerStubFixture, m *ocispec.Manifest) (desc ocispec.Descriptor, raw []byte) {
	t.Helper()
	m.MediaType = ocispec.MediaTypeImageManifest
	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	f.blobs[d.Digest] = raw
	return d, raw
}

// newReferrerStubFixture builds the artifact + signature fixture.
func newReferrerStubFixture(t *testing.T) *referrerStubFixture {
	t.Helper()
	f := &referrerStubFixture{blobs: map[digest.Digest][]byte{}}

	cfg := addStubBlob(f, []byte("{}"), ocispec.MediaTypeImageConfig)
	layer := addStubBlob(f, []byte("name: test-adapter\nprotocol: v2\n"), ocispec.MediaTypeImageLayer)
	f.artifactDesc, f.artifactRaw = addStubManifest(t, f, &ocispec.Manifest{
		Config: cfg,
		Layers: []ocispec.Descriptor{layer},
	})

	payload := addStubBlob(f, []byte("payload"), "application/vnd.dev.cosign.simplesigning.v1+json")
	f.payloadDesc = payload
	f.sigDesc, f.sigRaw = addStubManifest(t, f, &ocispec.Manifest{
		Config:  addStubBlob(f, []byte("{}"), ocispec.MediaTypeEmptyJSON),
		Layers:  []ocispec.Descriptor{payload},
		Subject: &f.artifactDesc,
	})

	index, err := json.Marshal(ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{f.sigDesc},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.indexRaw = index
	return f
}

// serveReferrerStub exposes the fixture over a minimal OCI distribution API.
// failure picks the injected referrer-copy failure mode.
func serveReferrerStub(t *testing.T, f *referrerStubFixture, failure referrerFailure) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v2/" + stubRepoPath + "/"
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		sub := strings.TrimPrefix(r.URL.Path, prefix)
		switch {
		case strings.HasPrefix(sub, "referrers/"):
			switch failure {
			case referrerDiscoveryFails:
				http.Error(w, "referrers unavailable", http.StatusInternalServerError)
				return
			case referrerCopySucceeds, referrerBlobCopyFails:
				w.Header().Set("Content-Type", ocispec.MediaTypeImageIndex)
				_, _ = w.Write(f.indexRaw)
			default:
				t.Fatalf("unhandled referrer failure mode: %d", failure)
			}
		case strings.HasPrefix(sub, "manifests/"):
			serveStubManifest(w, r, f, strings.TrimPrefix(sub, "manifests/"))
		case strings.HasPrefix(sub, "blobs/"):
			dg := digest.Digest(strings.TrimPrefix(sub, "blobs/"))
			if failure == referrerBlobCopyFails && dg == f.payloadDesc.Digest {
				http.NotFound(w, r)
				return
			}
			raw, ok := f.blobs[dg]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveStubManifest answers a manifest request for the stub tag or either
// manifest digest.
func serveStubManifest(w http.ResponseWriter, r *http.Request, f *referrerStubFixture, ref string) {
	var raw []byte
	switch ref {
	case stubTag, f.artifactDesc.Digest.String():
		raw = f.artifactRaw
	case f.sigDesc.Digest.String():
		raw = f.sigRaw
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
	_, _ = w.Write(raw)
}

// captureWarnLogs redirects slog.Default() to a buffer capturing warn-level
// records and restores the previous default on test cleanup.
func captureWarnLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// stubPuller builds a Puller over the stub registry and returns it together
// with its backing layout and the parsed reference of the stub artifact.
func stubPuller(t *testing.T, srv *httptest.Server) (*oci.Puller, *oci.Layout, oci.Reference) {
	t.Helper()
	layout, err := oci.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := oci.Parse(srv.Listener.Addr().String() + "/" + stubRepoPath + ":" + stubTag)
	if err != nil {
		t.Fatal(err)
	}
	return &oci.Puller{Layout: layout, PlainHTTP: true}, layout, ref
}

// TestPullWithAnnotations_ReferrerDiscoveryFailureIsLoggedAndStrictVerifyFailsClosed
// drives the strict-verification path with an injected referrer-copy failure
// (CRI-51): when the registry's Referrers API fails, the pull itself must stay
// best-effort and succeed, the copy failure must be logged with subject/repo
// context so a later missing-signature failure is diagnosable, and strict
// signature verification must still fail closed exactly as before.
func TestPullWithAnnotations_ReferrerDiscoveryFailureIsLoggedAndStrictVerifyFailsClosed(t *testing.T) {
	f := newReferrerStubFixture(t)
	srv := serveReferrerStub(t, f, referrerDiscoveryFails)
	logs := captureWarnLogs(t)

	puller, layout, ref := stubPuller(t, srv)
	dg, err := puller.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("pull must stay best-effort when referrer copy fails, got error: %v", err)
	}
	if dg != f.artifactDesc.Digest {
		t.Errorf("pull digest = %s, want %s", dg, f.artifactDesc.Digest)
	}

	logged := logs.String()
	wantRepo := "repo=" + srv.Listener.Addr().String() + "/" + stubRepoPath
	for _, want := range []string{
		"oci referrer copy failed",
		wantRepo,
		"subject=" + f.artifactDesc.Digest.String(),
		"response status code 500",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("referrer-copy failure log %q does not contain %q", logged, want)
		}
	}

	_, err = signing.Verify(context.Background(), layout, dg, signing.Policy{Mode: signing.ModeStrict})
	if err == nil {
		t.Fatal("strict verification must fail closed when the local referrer cache is incomplete")
	}
	if !strings.Contains(err.Error(), "no cosign signatures found") {
		t.Errorf("strict verification error = %q, want the missing-signature failure", err)
	}
}

// TestPullWithAnnotations_ReferrerBlobCopyFailureIsLogged pins the second
// copyReferrers failure branch (CRI-51): discovery succeeds but copying the
// signature referrer fails mid-copy; the pull stays best-effort and the
// wrapped per-referrer copy error is logged with context, and strict
// verification still fails closed.
func TestPullWithAnnotations_ReferrerBlobCopyFailureIsLogged(t *testing.T) {
	f := newReferrerStubFixture(t)
	srv := serveReferrerStub(t, f, referrerBlobCopyFails)
	logs := captureWarnLogs(t)

	puller, layout, ref := stubPuller(t, srv)
	dg, err := puller.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("pull must stay best-effort when a referrer blob copy fails, got error: %v", err)
	}
	if dg != f.artifactDesc.Digest {
		t.Errorf("pull digest = %s, want %s", dg, f.artifactDesc.Digest)
	}

	logged := logs.String()
	for _, want := range []string{
		"oci referrer copy failed",
		"oci: copy referrer " + f.sigDesc.Digest.String(),
		"not found",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("referrer-copy failure log %q does not contain %q", logged, want)
		}
	}

	if _, err := signing.Verify(context.Background(), layout, dg, signing.Policy{Mode: signing.ModeStrict}); err == nil {
		t.Fatal("strict verification must fail closed when the referrer copy failed mid-copy")
	}
}
