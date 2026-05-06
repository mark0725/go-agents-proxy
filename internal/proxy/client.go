package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mark0725/go-agents-proxy/internal/config"
)

var sharedHTTPClient *http.Client

func init() {
	sharedHTTPClient = &http.Client{
		Timeout: 5 * time.Minute,
	}
}

// SetProxyURL configures the HTTP client to use the given proxy URL.
func SetProxyURL(proxyURL string) {
	transport := &http.Transport{}
	if proxyURL != "" {
		transport.Proxy = func(*http.Request) (*url.URL, error) {
			return url.Parse(proxyURL)
		}
	}
	sharedHTTPClient.Transport = transport
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

// CallProvider sends a non-streaming request to a provider API.
func CallProvider(ctx context.Context, api config.APIConfig, body []byte, proxyURL string, endpoint string) (*http.Response, error) {
	slog.Debug("calling provider API", slog.String("url", endpoint), slog.String("api_type", api.APIType))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	switch api.APIType {
	case "anthropic":
		req.Header.Set("x-api-key", api.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+api.APIKey)
	}

	return doRequest(ctx, proxyURL, req)
}

// CallProviderStream sends a streaming request to a provider API.
func CallProviderStream(ctx context.Context, api config.APIConfig, body []byte, proxyURL string, endpoint string) (*http.Response, error) {
	slog.Debug("calling provider API stream", slog.String("url", endpoint), slog.String("api_type", api.APIType))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	switch api.APIType {
	case "anthropic":
		req.Header.Set("x-api-key", api.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+api.APIKey)
	}

	return doRequest(ctx, proxyURL, req)
}

// doRequest executes the HTTP request using the shared client or a per-request proxy.
func doRequest(ctx context.Context, proxyURL string, req *http.Request) (*http.Response, error) {
	if proxyURL != "" {
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: func(*http.Request) (*url.URL, error) {
					return url.Parse(proxyURL)
				},
			},
			Timeout: 5 * time.Minute,
		}
		return client.Do(req)
	}
	return sharedHTTPClient.Do(req)
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
