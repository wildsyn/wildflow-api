package inferenceclient

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSpeechSegmentsValidateSequenceAndContent(t *testing.T) {
	data := []byte(strings.Repeat("a", 100))
	segment := SpeechSegment{ID: "speech-1", AttemptID: "attempt-1", Sequence: 1, SizeBytes: 100, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), MediaType: "audio/wav"}
	corrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-WildFlow-Tenant-Ref") != "tenant-a" {
			t.Error("missing tenant")
		}
		if strings.HasSuffix(r.URL.Path, "/content") {
			if corrupt {
				_, _ = w.Write([]byte("bad"))
			} else {
				_, _ = w.Write(data)
			}
			return
		}
		body, _ := common.Marshal(SpeechSegmentList{Data: []SpeechSegment{segment}})
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "fixture-token", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	list, err := client.SpeechSegments(context.Background(), "job-1", "tenant-a")
	if err != nil || len(list.Data) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	if _, err := client.SpeechSegmentContent(context.Background(), "job-1", "tenant-a", segment); err != nil {
		t.Fatal(err)
	}
	corrupt = true
	if _, err := client.SpeechSegmentContent(context.Background(), "job-1", "tenant-a", segment); err == nil {
		t.Fatal("corrupt audio accepted")
	}
	segment.Sequence = 2
	if _, err := client.SpeechSegments(context.Background(), "job-1", "tenant-a"); err == nil {
		t.Fatal("out of order accepted")
	}
}
