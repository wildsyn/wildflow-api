package inferenceclient

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"io"
	"net/http"
	"net/url"
)

type SpeechSegment struct {
	ID        string `json:"segment_id"`
	AttemptID string `json:"attempt_id"`
	Sequence  int    `json:"sequence"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}
type SpeechSegmentList struct {
	Data []SpeechSegment `json:"data"`
}

func (client *Client) SpeechSegments(ctx context.Context, job, tenant string) (SpeechSegmentList, error) {
	var result SpeechSegmentList
	if err := validateScopedResource(job, tenant); err != nil {
		return result, err
	}
	request, err := client.scopedRequest(ctx, http.MethodGet, "/internal/v1/jobs/"+url.PathEscape(job)+"/audio-segments", tenant)
	if err != nil {
		return result, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return result, err
	}
	if response.StatusCode != 200 {
		return result, responseError(response, body)
	}
	if err := common.Unmarshal(body, &result); err != nil {
		return result, err
	}
	if len(result.Data) > 1024 {
		return SpeechSegmentList{}, errors.New("too many speech segments")
	}
	attempt := ""
	for i, segment := range result.Data {
		if i == 0 {
			attempt = segment.AttemptID
		}
		if !resourceIDPattern.MatchString(segment.ID) || !resourceIDPattern.MatchString(segment.AttemptID) || segment.AttemptID != attempt || segment.Sequence != i+1 || segment.MediaType != "audio/wav" || segment.SizeBytes < 44 || segment.SizeBytes > 16<<20 || !sha256Pattern.MatchString(segment.SHA256) {
			return SpeechSegmentList{}, errors.New("invalid speech segment")
		}
	}
	return result, nil
}
func (client *Client) SpeechSegmentContent(ctx context.Context, job, tenant string, segment SpeechSegment) ([]byte, error) {
	if err := validateScopedResource(job, tenant); err != nil {
		return nil, err
	}
	if !resourceIDPattern.MatchString(segment.ID) || segment.SizeBytes < 44 || segment.SizeBytes > 16<<20 || !sha256Pattern.MatchString(segment.SHA256) {
		return nil, errors.New("invalid speech segment")
	}
	request, err := client.scopedRequest(ctx, http.MethodGet, "/internal/v1/jobs/"+url.PathEscape(job)+"/audio-segments/"+url.PathEscape(segment.ID)+"/content", tenant)
	if err != nil {
		return nil, err
	}
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		body, _ := readBounded(response.Body)
		return nil, responseError(response, body)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, segment.SizeBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != segment.SizeBytes || fmt.Sprintf("%x", sha256.Sum256(data)) != segment.SHA256 {
		return nil, errors.New("speech segment integrity failure")
	}
	return data, nil
}
