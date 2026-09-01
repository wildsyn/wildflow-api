package controller

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ginParams(key string, value string) gin.Params {
	return gin.Params{{Key: key, Value: value}}
}

// TestTokenManagementStatusCodesMatchOpenAPI pins the ZXDW-203 finding-2
// contract: management token endpoints return real HTTP statuses (400 for
// validation, 404 for missing/foreign tokens) instead of a fixed 200.
func TestTokenManagementStatusCodesMatchOpenAPI(t *testing.T) {
	t.Run("AddToken rejects malformed body with 400", func(t *testing.T) {
		setupTokenControllerTestDB(t)
		ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", map[string]any{"unlimited_quota": "not-a-bool"}, 1)
		AddToken(ctx)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		assert.Equal(t, false, body["success"])
	})

	t.Run("AddToken rejects overlong name with 400", func(t *testing.T) {
		setupTokenControllerTestDB(t)
		name := string(make([]byte, 60))
		ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", newTokenPayload(0, name, ""), 1)
		AddToken(ctx)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})

	t.Run("GetTokenKey returns 404 for foreign token", func(t *testing.T) {
		db := setupTokenControllerTestDB(t)
		token := seedToken(t, db, 1, "foreign-key-token", "foreignkey0001")

		ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/"+strconv.Itoa(token.Id)+"/key", nil, 2)
		ctx.Params = ginParams("id", strconv.Itoa(token.Id))
		GetTokenKey(ctx)
		assert.Equal(t, http.StatusNotFound, recorder.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		assert.Equal(t, false, body["success"])

		// Owner still gets the key with 200.
		ownerCtx, ownerRecorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/"+strconv.Itoa(token.Id)+"/key", nil, 1)
		ownerCtx.Params = ginParams("id", strconv.Itoa(token.Id))
		GetTokenKey(ownerCtx)
		assert.Equal(t, http.StatusOK, ownerRecorder.Code)
	})

	t.Run("GetToken returns 404 for foreign token", func(t *testing.T) {
		db := setupTokenControllerTestDB(t)
		token := seedToken(t, db, 1, "foreign-token", "foreigntoken01")

		ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/"+strconv.Itoa(token.Id), nil, 2)
		ctx.Params = ginParams("id", strconv.Itoa(token.Id))
		GetToken(ctx)
		assert.Equal(t, http.StatusNotFound, recorder.Code)
	})

	t.Run("UpdateToken returns 404 for foreign token", func(t *testing.T) {
		db := setupTokenControllerTestDB(t)
		token := seedToken(t, db, 1, "foreign-update-token", "foreignupd0001")

		body := newTokenPayload(token.Id, "hijack-attempt", "")
		ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, 2)
		UpdateToken(ctx)
		assert.Equal(t, http.StatusNotFound, recorder.Code)

		var stored model.Token
		require.NoError(t, db.First(&stored, "id = ?", token.Id).Error)
		assert.Equal(t, "foreign-update-token", stored.Name, "foreign update must not modify the token")
	})

	t.Run("DeleteToken returns 404 for foreign token", func(t *testing.T) {
		db := setupTokenControllerTestDB(t)
		token := seedToken(t, db, 1, "foreign-delete-token", "foreigndel0001")

		ctx, recorder := newAuthenticatedContext(t, http.MethodDelete, "/api/token/"+strconv.Itoa(token.Id), nil, 2)
		ctx.Params = ginParams("id", strconv.Itoa(token.Id))
		DeleteToken(ctx)
		assert.Equal(t, http.StatusNotFound, recorder.Code)

		var count int64
		require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Count(&count).Error)
		assert.Equal(t, int64(1), count, "foreign delete must not remove the token")
	})

	t.Run("GetTokenKeysBatch rejects empty ids with 400", func(t *testing.T) {
		setupTokenControllerTestDB(t)
		ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/batch/keys", map[string]any{"ids": []int{}}, 1)
		GetTokenKeysBatch(ctx)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})

	t.Run("GetTokenKeysBatch rejects over-limit ids with 400", func(t *testing.T) {
		setupTokenControllerTestDB(t)
		ids := make([]int, 101)
		ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/batch/keys", map[string]any{"ids": ids}, 1)
		GetTokenKeysBatch(ctx)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})
}
