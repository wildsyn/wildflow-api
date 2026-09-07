package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

type TaskImageClient interface {
	StoreTaskImage(context.Context, string, string, []byte) (inferenceclient.TaskImage, error)
	ReadTaskImage(context.Context, string, inferenceclient.TaskImage) ([]byte, error)
}

type inferenceTaskArtifactStore struct{ client TaskImageClient }

func NewInferenceTaskArtifactStore(client TaskImageClient) TaskArtifactStore {
	return &inferenceTaskArtifactStore{client: client}
}

func (store *inferenceTaskArtifactStore) Enabled() bool { return store.client != nil }

// Ready verifies a tiny immutable object through the same tenant-scoped write
// and read path used for generated images. Repeated checks reuse one object per
// user, without adding probe jobs or billing records.
func (store *inferenceTaskArtifactStore) Ready(ctx context.Context, userID int) error {
	if !store.Enabled() {
		return ErrTaskArtifactStoreDisabled
	}
	if userID <= 0 {
		return errors.New("artifact owner required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var payload bytes.Buffer
	if err := png.Encode(&payload, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		return err
	}
	tenant := "user:" + strconv.Itoa(userID)
	ref, err := store.client.StoreTaskImage(ctx, tenant, "image/png", payload.Bytes())
	if err != nil {
		return err
	}
	stored, err := store.client.ReadTaskImage(ctx, tenant, ref)
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, payload.Bytes()) {
		return errors.New("artifact readiness content mismatch")
	}
	return nil
}

func (store *inferenceTaskArtifactStore) Resolve(task *model.Task, key string) (*StoredArtifactRef, error) {
	if task == nil {
		return nil, errors.New("task required")
	}
	ref, ok := task.PrivateData.StoredArtifacts[key]
	if !ok {
		return nil, nil
	}
	if ref.Backend != "inference" {
		return nil, errors.New("unsupported artifact backend")
	}
	return &ref, nil
}

// Persist mutates only the caller's task value. The polling owner must commit
// the reference with its completion CAS, after every required image is stored.
func (store *inferenceTaskArtifactStore) Persist(ctx context.Context, task *model.Task, artifact types.TaskArtifact, reader io.Reader) (*StoredArtifactRef, error) {
	if !store.Enabled() {
		return nil, ErrTaskArtifactStoreDisabled
	}
	if task == nil || task.UserId <= 0 || task.TaskID == "" || artifact.Type != "image" || artifact.Key == "" || reader == nil {
		return nil, errors.New("invalid image artifact")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > 32<<20 {
		return nil, errors.New("image artifact exceeds size limit")
	}
	digest := sha256.Sum256(payload)
	if existing, ok := task.PrivateData.StoredArtifacts[artifact.Key]; ok {
		if existing.Backend != "inference" || existing.ObjectKey != hex.EncodeToString(digest[:]) || existing.Size != int64(len(payload)) {
			return nil, errors.New("stored artifact cannot be replaced")
		}
		return &existing, nil
	}
	media := http.DetectContentType(payload)
	ref, err := store.client.StoreTaskImage(ctx, "user:"+strconv.Itoa(task.UserId), media, payload)
	if err != nil {
		return nil, err
	}
	stored := StoredArtifactRef{Backend: "inference", ObjectKey: ref.SHA256, MimeType: ref.MediaType, Size: ref.SizeBytes}
	if task.PrivateData.StoredArtifacts == nil {
		task.PrivateData.StoredArtifacts = make(map[string]model.StoredTaskArtifact)
	}
	task.PrivateData.StoredArtifacts[artifact.Key] = stored
	return &stored, nil
}

func (store *inferenceTaskArtifactStore) Serve(c *gin.Context, task *model.Task, ref *StoredArtifactRef) error {
	if !store.Enabled() {
		return ErrTaskArtifactStoreDisabled
	}
	if task == nil || task.UserId <= 0 || ref == nil || ref.Backend != "inference" {
		return errors.New("invalid stored artifact")
	}
	payload, err := store.client.ReadTaskImage(c.Request.Context(), "user:"+strconv.Itoa(task.UserId), inferenceclient.TaskImage{SHA256: ref.ObjectKey, SizeBytes: ref.Size, MediaType: ref.MimeType})
	if err != nil {
		return err
	}
	c.Header("Content-Type", ref.MimeType)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-cache")
	c.Header("ETag", `"`+ref.ObjectKey+`"`)
	http.ServeContent(c.Writer, c.Request, "image", time.Time{}, bytes.NewReader(payload))
	return nil
}
