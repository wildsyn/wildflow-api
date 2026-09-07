package inferenceclient

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestTaskArtifactClientPreservesTenantDigestAndBytes(t *testing.T) {
	payload := []byte("image fixture")
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		require.Equal(t, "tenant-9", r.Header.Get("X-WildFlow-Tenant-Ref"))
		require.Equal(t, "/internal/v1/task-artifacts/"+digest, r.URL.Path)
		if r.Method == http.MethodPut {
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, payload, body)
			data, err := common.Marshal(map[string]any{"sha256": digest, "size_bytes": len(payload), "media_type": "image/png"})
			require.NoError(t, err)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "fixture-token", AllowInternalHTTP: true, Timeout: time.Second})
	require.NoError(t, err)
	stored, err := client.StoreTaskImage(context.Background(), "tenant-9", "image/png", payload)
	require.NoError(t, err)
	require.Equal(t, digest, stored.SHA256)
	content, err := client.ReadTaskImage(context.Background(), "tenant-9", stored)
	require.NoError(t, err)
	require.Equal(t, payload, content)
	require.Equal(t, 2, calls)
	stored.SHA256 = "../escape"
	_, err = client.ReadTaskImage(context.Background(), "tenant-9", stored)
	require.Error(t, err)
	require.Equal(t, 2, calls)
}

func TestTaskArtifactClientRejectsCorruptedStoredBytes(t *testing.T) {
	expected := []byte("expected image")
	digest := fmt.Sprintf("%x", sha256.Sum256(expected))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("tampered image"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "fixture-token", AllowInternalHTTP: true, Timeout: time.Second})
	require.NoError(t, err)
	_, err = client.ReadTaskImage(context.Background(), "tenant-9", TaskImage{SHA256: digest, SizeBytes: int64(len(expected)), MediaType: "image/png"})
	require.Error(t, err)
}
