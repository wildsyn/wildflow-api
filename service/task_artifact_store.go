package service

import (
	"context"
	"errors"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"io"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// StoredArtifactRef describes a successfully persisted artifact object.
type StoredArtifactRef = model.StoredTaskArtifact

// TaskArtifactStore is the persistence boundary for generated artifact bytes.
// types.TaskArtifact is re-exported by relay/channel as channel.TaskArtifact.
type TaskArtifactStore interface {
	Enabled() bool
	Ready(context.Context, int) error
	Resolve(task *model.Task, artifactKey string) (*StoredArtifactRef, error)
	Persist(ctx context.Context, task *model.Task, artifact types.TaskArtifact, content io.Reader) (*StoredArtifactRef, error)
	Serve(c *gin.Context, task *model.Task, ref *StoredArtifactRef) error
}

var ErrTaskArtifactStoreDisabled = errors.New("task artifact store is disabled")

type disabledArtifactStore struct{}

func (disabledArtifactStore) Ready(context.Context, int) error { return ErrTaskArtifactStoreDisabled }

func (disabledArtifactStore) Enabled() bool {
	return false
}

func (disabledArtifactStore) Resolve(task *model.Task, key string) (*StoredArtifactRef, error) {
	if task != nil {
		if _, exists := task.PrivateData.StoredArtifacts[key]; exists {
			return nil, ErrTaskArtifactStoreDisabled
		}
	}
	return nil, nil
}

func (disabledArtifactStore) Persist(context.Context, *model.Task, types.TaskArtifact, io.Reader) (*StoredArtifactRef, error) {
	return nil, ErrTaskArtifactStoreDisabled
}

func (disabledArtifactStore) Serve(*gin.Context, *model.Task, *StoredArtifactRef) error {
	return ErrTaskArtifactStoreDisabled
}

var taskArtifactStore TaskArtifactStore = &disabledArtifactStore{}

// InitTaskArtifactStore is called once before request handlers and polling start.
// OSS credentials stay in inference; API reuses its existing internal identity.
func InitTaskArtifactStore() error {
	config := system_setting.LoadTaskArtifactStoreConfig()
	taskArtifactStore = &disabledArtifactStore{}
	if config.Mode != system_setting.TaskArtifactStoreModeInference {
		return nil
	}
	client, err := inferenceclient.New(inferenceclient.Config{
		BaseURL:           strings.TrimSpace(os.Getenv("WILDFLOW_INFERENCE_URL")),
		Token:             strings.TrimSpace(os.Getenv("WILDFLOW_INTERNAL_TOKEN")),
		Timeout:           30 * time.Second,
		AllowInternalHTTP: common.GetEnvOrDefaultBool("WILDFLOW_INFERENCE_ALLOW_INTERNAL_HTTP", false),
	})
	if err != nil {
		return errors.New("invalid inference artifact storage configuration")
	}
	taskArtifactStore = NewInferenceTaskArtifactStore(client)
	return nil
}

// GetTaskArtifactStore returns the startup-configured artifact storage backend.
func GetTaskArtifactStore() TaskArtifactStore {
	return taskArtifactStore
}
