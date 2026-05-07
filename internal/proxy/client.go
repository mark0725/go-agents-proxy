package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/mark0725/go-agents-proxy/internal/config"
)

var sharedHTTPClient = newHTTPClient("")

var excludedForwardHeaders = map[string]struct{}{
	"Accept-Encoding":      {},
	"Authorization":        {},
	"Connection":           {},
	"Content-Length":       {},
	"Keep-Alive":           {},
	"Proxy-Authenticate":   {},
	"Proxy-Authorization":  {},
	"Proxy-Connection":     {},
	"Te":                   {},
	"Trailer":              {},
	"Transfer-Encoding":    {},
	"Upgrade":              {},
	"X-Api-Key":            {},
}

func newHTTPClient(proxyURL string) *http.Client {
	return &http.Client{
		Transport: newTransport(proxyURL),
		Timeout:   5 * time.Minute,
	}
}

func newTransport(proxyURL string) *http.Transport {
	transport := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			if strings.TrimSpace(proxyURL) == "" {
				return nil, nil
			}
			return url.Parse(proxyURL)
		},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return transport
}

// BuildEndpoint joins baseURL with the request path suffix.
// If baseURL ends with "/v1" and suffix starts with "/v1", the duplicate "/v1" is
// removed so that configs like "https://api.anthropic.com/v1" work with
// client paths "/v1/messages" without producing "/v1/v1/messages".
func BuildEndpoint(baseURL, suffix string) string {
	if strings.HasSuffix(baseURL, "/v1") && strings.HasPrefix(suffix, "/v1") {
		return baseURL + suffix[len("/v1"):]
	}
	return baseURL + suffix
}

func shouldForwardHeader(name string) bool {
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	_, excluded := excludedForwardHeaders[canonical]
	return !excluded
}

func copyForwardHeaders(dst, src http.Header) {
	for name, values := range src {
		if !shouldForwardHeader(name) {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func buildProviderRequest(ctx context.Context, api config.APIConfig, body []byte, endpoint string, acceptStream bool, originalHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	copyForwardHeaders(req.Header, originalHeaders)
	req.Header.Set("Content-Type", "application/json")
	if acceptStream {
		req.Header.Set("Accept", "text/event-stream")
	}
	switch api.APIType {
	case "anthropic":
		req.Header.Set("x-api-key", api.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+api.APIKey)
	}
	return req, nil
}

func doRequest(req *http.Request, proxyURL string) (*http.Response, error) {
	client := sharedHTTPClient
	if strings.TrimSpace(proxyURL) != "" {
		client = newHTTPClient(proxyURL)
	}
	return client.Do(req)
}

// CallProvider sends a non-streaming request to a provider API.
func CallProvider(ctx context.Context, api config.APIConfig, body []byte, proxyURL string, endpoint string, originalHeaders http.Header) (*http.Response, error) {
	slog.Debug("calling provider API", slog.String("url", endpoint), slog.String("api_type", api.APIType))
	req, err := buildProviderRequest(ctx, api, body, endpoint, false, originalHeaders)
	if err != nil {
		return nil, err
	}
	return doRequest(req, proxyURL)
}

// CallProviderStream sends a streaming request to a provider API.
func CallProviderStream(ctx context.Context, api config.APIConfig, body []byte, proxyURL string, endpoint string, originalHeaders http.Header) (*http.Response, error) {
	slog.Debug("calling provider API stream", slog.String("url", endpoint), slog.String("api_type", api.APIType))
	req, err := buildProviderRequest(ctx, api, body, endpoint, true, originalHeaders)
	if err != nil {
		return nil, err
	}
	return doRequest(req, proxyURL)
}

// ReadResponseBody reads the full response body and returns it.
func ReadResponseBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// IsRetryableError returns true if the error or status code indicates a retryable failure.
func IsRetryableError(err error, statusCode int) bool {
	if err != nil {
		return true
	}
	return statusCode >= 500 || statusCode == 429
}
