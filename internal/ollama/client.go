package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	http      *http.Client
	baseURL   string
	apiKey    string
	webAPIURL string
}

func NewClient(baseURL string) *Client {
	base := strings.TrimRight(baseURL, "/")
	return &Client{
		http:      &http.Client{Timeout: 180 * time.Second},
		baseURL:   base,
		apiKey:    os.Getenv("OLLAMA_API_KEY"),
		webAPIURL: "https://ollama.com",
	}
}

func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	url := c.baseURL + "/api/chat"
	if !strings.HasPrefix(url, "http") {
		url = "http://localhost:11434/api/chat"
	}
	b, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal chat request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return ChatResponse{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(hreq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("chat request failed: %w", err)
	}
	defer res.Body.Close()
	respBody, err := io.ReadAll(res.Body)
	if err != nil {
		return ChatResponse{}, err
	}
	if res.StatusCode >= 300 {
		return ChatResponse{}, fmt.Errorf("chat request status %d: %s", res.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out ChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return ChatResponse{}, fmt.Errorf("decode chat response: %w", err)
	}
	return out, nil
}

func (c *Client) WebSearch(ctx context.Context, query string, maxResults int) (WebSearchResponse, error) {
	if c.apiKey == "" {
		return WebSearchResponse{}, fmt.Errorf("OLLAMA_API_KEY is required for web_search")
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 10 {
		maxResults = 10
	}
	payload := map[string]interface{}{
		"query":       query,
		"max_results": maxResults,
	}
	return doWebRequest[WebSearchResponse](ctx, c.http, c.webAPIURL+"/api/web_search", c.apiKey, payload)
}

func (c *Client) WebFetch(ctx context.Context, url string) (WebFetchResponse, error) {
	if c.apiKey == "" {
		return WebFetchResponse{}, fmt.Errorf("OLLAMA_API_KEY is required for web_fetch")
	}
	payload := map[string]interface{}{
		"url": url,
	}
	return doWebRequest[WebFetchResponse](ctx, c.http, c.webAPIURL+"/api/web_fetch", c.apiKey, payload)
}

func doWebRequest[T any](ctx context.Context, client *http.Client, endpoint, apiKey string, payload any) (T, error) {
	var zero T
	b, err := json.Marshal(payload)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := client.Do(req)
	if err != nil {
		return zero, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return zero, err
	}
	if res.StatusCode >= 300 {
		return zero, fmt.Errorf("web api status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return zero, err
	}
	return out, nil
}
