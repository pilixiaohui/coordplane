package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

const maxResponseBytes = 2 << 20

type ClientOption func(*Client)

func WithBearerToken(token string) ClientOption {
	return func(client *Client) {
		client.token = strings.TrimSpace(token)
	}
}

// Client is an HTTP/JSON client whose transport can dial only one Unix socket.
type Client struct {
	httpClient *http.Client
	transport  *http.Transport
	token      string
}

func NewUnixClient(socketPath string, options ...ClientOption) (*Client, error) {
	if !filepath.IsAbs(socketPath) {
		return nil, errors.New("transport: Unix socket path must be absolute")
	}
	socketPath = filepath.Clean(socketPath)
	dialer := &net.Dialer{}
	httpTransport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &Client{
		httpClient: &http.Client{Transport: httpTransport},
		transport:  httpTransport,
	}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client, nil
}

// JSON invokes one fixed path and decodes only the envelope's data field into
// output. A rejected envelope is returned as *core.Error.
func (c *Client) JSON(ctx context.Context, method, path string, input, output any) error {
	if c == nil || c.httpClient == nil {
		return errors.New("transport: Unix client is not initialized")
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.IsAbs() || !strings.HasPrefix(path, "/") {
		return errors.New("transport: request path must be an absolute-path reference")
	}
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("transport: encode JSON request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return fmt.Errorf("transport: create Unix request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("transport: Unix request: %w", err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("transport: decode JSON envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("transport: JSON envelope contains trailing content")
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if !envelope.OK {
		return errors.New("transport: rejected envelope omitted error")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("transport: successful envelope used HTTP status %d", response.StatusCode)
	}
	if output == nil || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		return fmt.Errorf("transport: decode JSON response data: %w", err)
	}
	return nil
}

func (c *Client) CloseIdleConnections() {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}
