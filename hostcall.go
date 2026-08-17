package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// hostLog forwards a structured log line to the host logger.
func hostLog(level, message string, fields map[string]any) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
}

type hostHTTPDoRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

type hostHTTPStreamStartResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostHTTPClient issues upstream requests through the host HTTP bridge so proxy
// configuration, transport policy, and request logging stay under host control.
// It satisfies the connect.HTTPClient interface.
type hostHTTPClient struct{}

// Do sends req through the host streaming HTTP bridge and returns a response
// whose body is read lazily, which keeps Connect server streams incremental.
func (hostHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("devin: nil request")
	}
	var body []byte
	if req.Body != nil {
		defer func() {
			if errClose := req.Body.Close(); errClose != nil {
				hostLog("debug", "devin: close request body", map[string]any{"error": errClose.Error()})
			}
		}()
		read, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, fmt.Errorf("devin: read request body: %w", errRead)
		}
		body = read
	}
	raw, errCall := callHost(pluginabi.MethodHostHTTPDoStream, hostHTTPDoRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header,
		Body:    body,
	})
	if errCall != nil {
		return nil, errCall
	}
	var start hostHTTPStreamStartResponse
	if errDecode := json.Unmarshal(raw, &start); errDecode != nil {
		return nil, fmt.Errorf("devin: decode host stream response: %w", errDecode)
	}
	if strings.TrimSpace(start.StreamID) == "" {
		return nil, fmt.Errorf("devin: host stream response missing stream_id")
	}
	header := start.Headers
	if header == nil {
		header = http.Header{}
	}
	resp := &http.Response{
		Status:        http.StatusText(start.StatusCode),
		StatusCode:    start.StatusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          &hostStreamBody{streamID: start.StreamID},
		ContentLength: -1,
		Request:       req,
	}
	return resp, nil
}

// hostStreamBody adapts host stream reads to an io.ReadCloser.
type hostStreamBody struct {
	streamID string

	mu     sync.Mutex
	buf    bytes.Buffer
	done   bool
	closed bool
	err    error
}

// Read returns buffered payload bytes, pulling more host chunks when needed.
func (b *hostStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if b.done || b.closed {
			return 0, io.EOF
		}
		if errFill := b.fillLocked(); errFill != nil {
			b.err = errFill
			if b.buf.Len() == 0 {
				return 0, errFill
			}
			break
		}
	}
	return b.buf.Read(p)
}

// fillLocked reads exactly one chunk from the host stream into the buffer.
func (b *hostStreamBody) fillLocked() error {
	raw, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": b.streamID})
	if errCall != nil {
		return errCall
	}
	var chunk hostHTTPStreamReadResponse
	if errDecode := json.Unmarshal(raw, &chunk); errDecode != nil {
		return fmt.Errorf("devin: decode host stream chunk: %w", errDecode)
	}
	if strings.TrimSpace(chunk.Error) != "" {
		return fmt.Errorf("%s", chunk.Error)
	}
	if len(chunk.Payload) > 0 {
		b.buf.Write(chunk.Payload)
	}
	if chunk.Done {
		b.done = true
		if b.buf.Len() == 0 {
			return io.EOF
		}
	}
	return nil
}

// Close releases the host-side stream.
func (b *hostStreamBody) Close() error {
	b.mu.Lock()
	alreadyClosed := b.closed
	b.closed = true
	b.mu.Unlock()
	if alreadyClosed {
		return nil
	}
	_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": b.streamID})
	return nil
}
