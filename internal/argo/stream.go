package argo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Stream calls a grpc-gateway streaming endpoint (logs) and passes each
// result to fn until the server ends the stream, fn returns false, or
// maxBytes have been read. kind selects the credential (CD or Workflows).
func (s *Sessions) Stream(ctx context.Context, i Instance, path string, maxBytes int, fn func(json.RawMessage) bool) error {
	var token string
	if i.Kind == CD {
		t, err := s.CDToken(ctx, i)
		if err != nil {
			return err
		}
		token = t
	} else {
		c, err := s.credential(i)
		if err != nil {
			return err
		}
		if c.Method == "" {
			return &NeedsLogin{Instance: i, Reason: "no token configured"}
		}
		token = c.Token
	}
	hc, err := HTTPClient(i)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.URL+path, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", i.ID(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		s.setRejected(i, true)
		return &NeedsLogin{Instance: i, Reason: "the server rejected the token"}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	read := 0
	for sc.Scan() {
		line := sc.Bytes()
		read += len(line)
		if resp.StatusCode/100 != 2 {
			return &APIError{Status: resp.StatusCode, Message: apiMessage(line)}
		}
		var msg struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if msg.Error != nil {
			return errors.New(strings.TrimSpace(msg.Error.Message))
		}
		if len(msg.Result) > 0 && !fn(msg.Result) {
			return nil
		}
		if read > maxBytes {
			return nil
		}
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode}
	}
	return sc.Err()
}
