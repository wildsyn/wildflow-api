package inferenceclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

const maximumTaskImageBytes = 32 << 20

// TaskImage is an immutable reference; task state remains in the API database.
type TaskImage struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

func (client *Client) StoreTaskImage(ctx context.Context, tenant, mediaType string, payload []byte) (TaskImage, error) {
	if len(payload) == 0 || len(payload) > maximumTaskImageBytes {
		return TaskImage{}, errors.New("invalid task image size")
	}
	sum := sha256.Sum256(payload)
	expected := TaskImage{SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(payload)), MediaType: mediaType}
	if err := validateTaskImage(tenant, expected); err != nil {
		return TaskImage{}, err
	}
	request, err := client.scopedRequest(ctx, http.MethodPut, "/internal/v1/task-artifacts/"+expected.SHA256, tenant)
	if err != nil {
		return TaskImage{}, err
	}
	request.Body = io.NopCloser(bytes.NewReader(payload))
	request.ContentLength = int64(len(payload))
	request.Header.Set("Content-Type", mediaType)
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		return TaskImage{}, err
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return TaskImage{}, err
	}
	if response.StatusCode != http.StatusOK {
		return TaskImage{}, responseError(response, body)
	}
	var actual TaskImage
	if err = common.Unmarshal(body, &actual); err != nil {
		return TaskImage{}, err
	}
	if actual != expected {
		return TaskImage{}, errors.New("stored task image metadata mismatch")
	}
	return actual, nil
}

func (client *Client) ReadTaskImage(ctx context.Context, tenant string, ref TaskImage) ([]byte, error) {
	if err := validateTaskImage(tenant, ref); err != nil {
		return nil, err
	}
	request, err := client.scopedRequest(ctx, http.MethodGet, "/internal/v1/task-artifacts/"+ref.SHA256, tenant)
	if err != nil {
		return nil, err
	}
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, readErr := readBounded(response.Body)
		if readErr != nil {
			return nil, readErr
		}
		return nil, responseError(response, body)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, ref.SizeBytes+1))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	if int64(len(body)) != ref.SizeBytes || hex.EncodeToString(digest[:]) != ref.SHA256 || response.Header.Get("Content-Type") != ref.MediaType {
		return nil, errors.New("stored task image content mismatch")
	}
	return body, nil
}

func validateTaskImage(tenant string, ref TaskImage) error {
	if err := validateScopedResource(ref.SHA256, tenant); err != nil {
		return err
	}
	if !sha256Pattern.MatchString(ref.SHA256) || ref.SHA256 != strings.ToLower(ref.SHA256) || ref.SizeBytes < 1 || ref.SizeBytes > maximumTaskImageBytes {
		return errors.New("invalid task image reference")
	}
	if ref.MediaType != "image/png" && ref.MediaType != "image/jpeg" && ref.MediaType != "image/webp" {
		return errors.New("unsupported task image media type")
	}
	return nil
}
