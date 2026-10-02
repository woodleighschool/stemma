package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoveryCacheSharesAndRevalidatesByCredential(t *testing.T) {
	var requests atomic.Int32
	var conditional atomic.Int32
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail {
			http.Error(w, "failed", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("If-None-Match") == `"metadata"` {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"metadata"`)
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	m := manager(t)
	read := func(manager *Manager, token string) (string, error) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		req.Header.Set("Authorization", token)
		res, err := manager.metadataClient().Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return "", fmt.Errorf("HTTP %d", res.StatusCode)
		}
		data, err := io.ReadAll(res.Body)
		return string(data), err
	}
	for range 2 {
		if body, err := read(m, "first"); err != nil || body != "first" {
			t.Fatalf("first: %s %v", body, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("shared requests=%d", requests.Load())
	}
	next := New(m.Store, m.Root, false)
	if body, err := read(next, "first"); err != nil || body != "first" {
		t.Fatalf("revalidation: %s %v", body, err)
	}
	if requests.Load() != 2 || conditional.Load() != 1 {
		t.Fatalf("persistent requests=%d", requests.Load())
	}
	if body, err := read(next, "second"); err != nil || body != "second" {
		t.Fatalf("credential collision: %s %v", body, err)
	}
	fail = true
	fresh := New(m.Store, m.Root, false)
	if _, err := read(fresh, "first"); err == nil {
		t.Fatal("failed refresh silently reused stale metadata")
	}
}

func TestDiscoveryCacheWaiterCanCancel(t *testing.T) {
	m := manager(t)
	started, release := make(chan struct{}), make(chan struct{})
	m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("metadata"))}, nil
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://vendor.test/meta", nil)
	first := make(chan error, 1)
	go func() {
		res, err := m.metadataClient().Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
		first <- err
	}()
	defer func() { close(release); <-first }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() {
		res, err := m.metadataClient().Do(req.Clone(ctx))
		if res != nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled follower waited for the other request")
	}
}
