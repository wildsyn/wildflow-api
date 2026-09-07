package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/gin-gonic/gin"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

type taskImageClientFixture struct {
	fail     bool
	readFail bool
	tenant   string
	payload  []byte
}

func (f *taskImageClientFixture) StoreTaskImage(_ context.Context, tenant, media string, payload []byte) (inferenceclient.TaskImage, error) {
	f.tenant = tenant
	if f.fail {
		return inferenceclient.TaskImage{}, errors.New("storage unavailable")
	}
	f.payload = append([]byte(nil), payload...)
	digest := sha256.Sum256(payload)
	return inferenceclient.TaskImage{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(payload)), MediaType: media}, nil
}
func (f *taskImageClientFixture) ReadTaskImage(_ context.Context, tenant string, _ inferenceclient.TaskImage) ([]byte, error) {
	f.tenant = tenant
	if f.fail || f.readFail {
		return nil, errors.New("storage unavailable")
	}
	return f.payload, nil
}

func TestTaskImageReferenceAppearsOnlyAfterStorageSucceeds(t *testing.T) {
	var content bytes.Buffer
	require.NoError(t, png.Encode(&content, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	client := &taskImageClientFixture{fail: true}
	store := NewInferenceTaskArtifactStore(client)
	task := &model.Task{TaskID: "task-image", UserId: 9}
	artifact := types.TaskArtifact{Key: "image-0", Type: "image"}
	_, err := store.Persist(t.Context(), task, artifact, bytes.NewReader(content.Bytes()))
	require.Error(t, err)
	ref, err := store.Resolve(task, "image-0")
	require.NoError(t, err)
	require.Nil(t, ref)
	client.fail = false
	ref, err = store.Persist(t.Context(), task, artifact, bytes.NewReader(content.Bytes()))
	require.NoError(t, err)
	require.NotNil(t, ref)
	require.Equal(t, "user:9", client.tenant)
	// Simulate database JSON serialization and a later read without a provider.
	saved, err := task.PrivateData.Value()
	require.NoError(t, err)
	restored := &model.Task{TaskID: task.TaskID, UserId: task.UserId}
	require.NoError(t, restored.PrivateData.Scan(saved))
	got, err := store.Resolve(restored, "image-0")
	require.NoError(t, err)
	require.Equal(t, ref, got)
	missing, err := store.Resolve(restored, "image-1")
	require.NoError(t, err)
	require.Nil(t, missing)
}

func TestStoredImageReadAndImmutability(t *testing.T) {
	var content bytes.Buffer
	require.NoError(t, png.Encode(&content, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	client := &taskImageClientFixture{}
	store := NewInferenceTaskArtifactStore(client)
	task := &model.Task{TaskID: "image-task", UserId: 17}
	artifact := types.TaskArtifact{Key: "image-0", Type: "image"}
	ref, err := store.Persist(t.Context(), task, artifact, bytes.NewReader(content.Bytes()))
	require.NoError(t, err)
	replay, err := store.Persist(t.Context(), task, artifact, bytes.NewReader(content.Bytes()))
	require.NoError(t, err)
	require.Equal(t, ref, replay)
	_, err = store.Persist(t.Context(), task, artifact, bytes.NewReader([]byte("replacement")))
	require.ErrorContains(t, err, "cannot be replaced")
	require.Equal(t, *ref, task.PrivateData.StoredArtifacts[artifact.Key])
	_, err = (disabledArtifactStore{}).Resolve(task, artifact.Key)
	require.ErrorIs(t, err, ErrTaskArtifactStoreDisabled)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(method, "/artifact", nil)
		ctx.Request.Header.Set("Range", "bytes=0-7")
		require.NoError(t, store.Serve(ctx, task, ref))
		if method == http.MethodGet {
			require.Equal(t, http.StatusPartialContent, recorder.Code)
		} else {
			require.Equal(t, http.StatusOK, recorder.Code)
		}
		require.Equal(t, "user:17", client.tenant)
		require.Equal(t, "image/png", recorder.Header().Get("Content-Type"))
		if method == http.MethodGet {
			require.Equal(t, content.Bytes()[:8], recorder.Body.Bytes())
		} else {
			require.Empty(t, recorder.Body.Bytes())
		}
	}
	client.fail = true
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/artifact", nil)
	require.Error(t, store.Serve(ctx, task, ref))
	require.False(t, ctx.Writer.Written())
}

func TestImageStorageAdmissionChecksBothWriteAndRead(t *testing.T) {
	client := &taskImageClientFixture{}
	store := NewInferenceTaskArtifactStore(client)
	require.NoError(t, store.Ready(t.Context(), 9))
	require.Equal(t, "user:9", client.tenant)
	require.Error(t, store.Ready(t.Context(), 0))
	client.fail = true
	require.Error(t, store.Ready(t.Context(), 9))
	client.fail = false
	client.readFail = true
	require.Error(t, store.Ready(t.Context(), 9))
	require.ErrorIs(t, (disabledArtifactStore{}).Ready(t.Context(), 9), ErrTaskArtifactStoreDisabled)
}

func TestInferenceArtifactStorageStartupUsesExistingIdentity(t *testing.T) {
	original := taskArtifactStore
	t.Cleanup(func() { taskArtifactStore = original })
	t.Setenv("TASK_ARTIFACT_STORE_MODE", "inference")
	t.Setenv("WILDFLOW_INFERENCE_URL", "https://inference.example.com")
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "test-internal-token")
	require.NoError(t, InitTaskArtifactStore())
	require.True(t, GetTaskArtifactStore().Enabled())
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "")
	require.Error(t, InitTaskArtifactStore())
	require.False(t, GetTaskArtifactStore().Enabled())
	t.Setenv("TASK_ARTIFACT_STORE_MODE", "upstream")
	require.NoError(t, InitTaskArtifactStore())
	require.False(t, GetTaskArtifactStore().Enabled())
}
