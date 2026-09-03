package common

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/QuantumNous/new-api/common"
)

// NewOutboundJSONBody wraps the already-marshaled upstream request body into a
// BodyStorage. When disk cache is enabled and the payload exceeds the configured
// threshold, the data is written to a temp file and the original []byte can be
// GC'd, significantly reducing the heap residency while waiting for the
// upstream provider to respond (the dominant cost for large base64 payloads).
//
// In memory mode the underlying memoryStorage reuses the same backing array,
// so this is equivalent to bytes.NewReader(data) in terms of memory usage.
//
// The caller MUST invoke closer.Close() once the upstream call has finished
// (typically via defer) to release the disk file / memory accounting.
//
// The returned body exposes its size and replay capability without exposing
// io.Closer. Request construction uses that metadata to populate ContentLength
// and GetBody, while the caller retains ownership of the underlying storage
// through the separately returned closer.
func NewOutboundJSONBody(data []byte) (body common.ReplayableBody, closer io.Closer, err error) {
	storage, err := common.CreateBodyStorage(data)
	if err != nil {
		return nil, nil, err
	}
	return common.NewReplayableBodyReader(storage), storage, nil
}

// NewPassThroughJSONBody returns the original pass-through payload unless an
// omitted optional integer needs an executable default. In that case it adds
// only the missing field, preserving every other client-supplied JSON field.
// A JSON null is treated as omitted, while an explicit zero is preserved.
func NewPassThroughJSONBody(storage common.BodyStorage, field string, value *uint) (body io.Reader, closer io.Closer, err error) {
	if value == nil {
		return common.NewReplayableBodyReader(storage), nil, nil
	}

	rawBody, err := storage.Bytes()
	if err != nil {
		return nil, nil, err
	}
	fields := map[string]json.RawMessage{}
	if err := common.Unmarshal(rawBody, &fields); err != nil {
		return nil, nil, err
	}
	if raw, exists := fields[field]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return common.NewReplayableBodyReader(storage), nil, nil
	}
	encodedValue, err := common.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	fields[field] = encodedValue
	jsonData, err := common.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	return NewOutboundJSONBody(jsonData)
}
