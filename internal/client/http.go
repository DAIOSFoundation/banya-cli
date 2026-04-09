package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// HTTPClient communicates with the code agent server over HTTP + SSE.
type HTTPClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewHTTPClient creates a new HTTP-based API client.
func NewHTTPClient(baseURL, apiKey string) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 0, // no timeout for streaming
		},
	}
}

// SendMessage sends a chat request and streams back server events via SSE.
func (c *HTTPClient) SendMessage(req protocol.ChatRequest) (<-chan protocol.ServerEvent, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v1/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(errBody))
	}

	events := make(chan protocol.ServerEvent, 64)
	go c.readSSE(resp.Body, events)

	return events, nil
}

// readSSE parses the SSE stream and sends events to the channel.
func (c *HTTPClient) readSSE(body io.ReadCloser, events chan<- protocol.ServerEvent) {
	defer close(events)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1MB per line

	var eventType string
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// empty line = event boundary
			if len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				var evt protocol.ServerEvent
				if err := json.Unmarshal([]byte(data), &evt); err == nil {
					if eventType != "" {
						evt.Type = protocol.EventType(eventType)
					}
					events <- evt
				}
			}
			eventType = ""
			dataLines = nil
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data:"))
		}
		// ignore id:, retry:, and comments (:)
	}
}

// SendApproval responds to a tool-call approval request.
func (c *HTTPClient) SendApproval(resp protocol.ApprovalResponse) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal approval: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v1/approval", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	c.setHeaders(httpReq)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send approval: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("server returned %d: %s", httpResp.StatusCode, string(errBody))
	}

	return nil
}

// HealthCheck verifies server connectivity.
func (c *HTTPClient) HealthCheck() error {
	httpReq, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/v1/health", nil)
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// Close cleans up the HTTP client resources.
func (c *HTTPClient) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

func (c *HTTPClient) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("User-Agent", "banya-cli/0.1.0")
}
