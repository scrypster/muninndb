package muninn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout      = 5 * time.Second
	defaultMaxRetries   = 3
	defaultRetryBackoff = 500 * time.Millisecond
)

// Client is the MuninnDB REST API client.
type Client struct {
	baseURL      string
	token        string
	httpClient   *http.Client
	timeout      time.Duration
	maxRetries   int
	retryBackoff time.Duration
}

// NewClient creates a new MuninnDB client.
func NewClient(baseURL, token string) *Client {
	return NewClientWithOptions(baseURL, token, defaultTimeout, defaultMaxRetries, defaultRetryBackoff)
}

// NewClientWithOptions creates a new MuninnDB client with custom options.
func NewClientWithOptions(baseURL, token string, timeout time.Duration, maxRetries int, retryBackoff time.Duration) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		token:        token,
		httpClient:   &http.Client{Timeout: timeout},
		timeout:      timeout,
		maxRetries:   maxRetries,
		retryBackoff: retryBackoff,
	}
}

// Write writes an engram to the vault.
func (c *Client) Write(ctx context.Context, vault, concept, content string, tags []string) (string, error) {
	req := WriteRequest{
		Vault:      vault,
		Concept:    concept,
		Content:    content,
		Tags:       tags,
		Confidence: 0.9,
		Stability:  0.5,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	var resp WriteResponse
	if err := c.request(ctx, "POST", "/api/engrams?"+q.Encode(), body, &resp); err != nil {
		return "", err
	}

	return resp.ID, nil
}

// WriteWithOptions writes an engram with full control over all fields.
// The caller is responsible for setting Confidence and Stability; no defaults
// are applied (unlike Write, which hard-codes 0.9 and 0.5).
func (c *Client) WriteWithOptions(ctx context.Context, req WriteRequest) (*WriteResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", req.Vault)

	var resp WriteResponse
	if err := c.request(ctx, "POST", "/api/engrams?"+q.Encode(), body, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// WriteBatch writes multiple engrams in a single batch call. Maximum 50 per batch.
func (c *Client) WriteBatch(ctx context.Context, vault string, engrams []WriteRequest) (*BatchWriteResponse, error) {
	if len(engrams) == 0 {
		return nil, fmt.Errorf("engrams list must not be empty")
	}
	if len(engrams) > 50 {
		return nil, fmt.Errorf("batch size exceeds maximum of 50")
	}

	for i := range engrams {
		if engrams[i].Vault == "" {
			engrams[i].Vault = vault
		}
	}

	payload := struct {
		Engrams []WriteRequest `json:"engrams"`
	}{Engrams: engrams}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	var resp BatchWriteResponse
	if err := c.request(ctx, "POST", "/api/engrams/batch?"+q.Encode(), body, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// Read reads an engram by ID.
func (c *Client) Read(ctx context.Context, id, vault string) (*Engram, error) {
	q := url.Values{}
	q.Set("vault", vault)
	path := fmt.Sprintf("/api/engrams/%s?%s", id, q.Encode())

	engram := &Engram{}
	if err := c.request(ctx, "GET", path, nil, engram); err != nil {
		return nil, err
	}

	return engram, nil
}

// Activate activates memory based on context.
func (c *Client) Activate(ctx context.Context, vault string, context []string, maxResults int) (*ActivateResponse, error) {
	req := ActivateRequest{
		Vault:      vault,
		Context:    context,
		MaxResults: maxResults,
		Threshold:  0.1,
		MaxHops:    0,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	resp := &ActivateResponse{}
	if err := c.request(ctx, "POST", "/api/activate?"+q.Encode(), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Link links two engrams.
func (c *Client) Link(ctx context.Context, vault, sourceID, targetID string, relType int, weight float64) error {
	req := LinkRequest{
		Vault:    vault,
		SourceID: sourceID,
		TargetID: targetID,
		RelType:  relType,
		Weight:   weight,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	return c.request(ctx, "POST", "/api/link?"+q.Encode(), body, nil)
}

// Forget forgets an engram.
func (c *Client) Forget(ctx context.Context, id, vault string) error {
	q := url.Values{}
	q.Set("vault", vault)
	path := fmt.Sprintf("/api/engrams/%s?%s", id, q.Encode())

	return c.request(ctx, "DELETE", path, nil, nil)
}

// Stats gets database statistics. Pass an empty vault to get global stats.
func (c *Client) Stats(ctx context.Context, vault string) (*StatsResponse, error) {
	path := "/api/stats"
	if vault != "" {
		q := url.Values{}
		q.Set("vault", vault)
		path += "?" + q.Encode()
	}
	resp := &StatsResponse{}
	if err := c.request(ctx, "GET", path, nil, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Subscribe subscribes to vault events via Server-Sent Events.
func (c *Client) Subscribe(ctx context.Context, vault string) (<-chan Push, error) {
	q := url.Values{}
	q.Set("vault", vault)
	q.Set("push_on_write", "true")
	url := fmt.Sprintf("%s/api/subscribe?%s", c.baseURL, q.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.addHeaders(req)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SSE stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("subscription failed with status %d", resp.StatusCode)
	}

	ch := make(chan Push)

	go func() {
		defer func() {
			resp.Body.Close()
			close(ch)
		}()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}

			data := string(line[6:])
			var push Push
			if err := json.Unmarshal([]byte(data), &push); err != nil {
				continue
			}

			select {
			case ch <- push:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// Health checks if the server is healthy.
func (c *Client) Health(ctx context.Context) (bool, error) {
	err := c.request(ctx, "GET", "/api/health", nil, nil)
	return err == nil, err
}

// ActivateWithOptions activates memory with full control over all fields.
func (c *Client) ActivateWithOptions(ctx context.Context, req ActivateRequest) (*ActivateResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", req.Vault)

	resp := &ActivateResponse{}
	if err := c.request(ctx, "POST", "/api/activate?"+q.Encode(), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Evolve evolves an engram's content, creating a new version.
func (c *Client) Evolve(ctx context.Context, vault, engramID, newContent, reason string) (*EvolveResponse, error) {
	payload := struct {
		NewContent string `json:"new_content"`
		Reason     string `json:"reason"`
		Vault      string `json:"vault"`
	}{NewContent: newContent, Reason: reason, Vault: vault}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	resp := &EvolveResponse{}
	if err := c.request(ctx, "POST", fmt.Sprintf("/api/engrams/%s/evolve?%s", engramID, q.Encode()), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Consolidate merges multiple engrams into one.
func (c *Client) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*ConsolidateResponse, error) {
	payload := struct {
		Vault         string   `json:"vault"`
		IDs           []string `json:"ids"`
		MergedContent string   `json:"merged_content"`
	}{Vault: vault, IDs: ids, MergedContent: mergedContent}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	resp := &ConsolidateResponse{}
	if err := c.request(ctx, "POST", "/api/consolidate?"+q.Encode(), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Decide records a decision as an engram.
func (c *Client) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*DecideResponse, error) {
	payload := struct {
		Vault        string   `json:"vault"`
		Decision     string   `json:"decision"`
		Rationale    string   `json:"rationale"`
		Alternatives []string `json:"alternatives,omitempty"`
		EvidenceIDs  []string `json:"evidence_ids,omitempty"`
	}{Vault: vault, Decision: decision, Rationale: rationale, Alternatives: alternatives, EvidenceIDs: evidenceIDs}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	resp := &DecideResponse{}
	if err := c.request(ctx, "POST", "/api/decide?"+q.Encode(), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Restore restores a soft-deleted engram.
func (c *Client) Restore(ctx context.Context, id, vault string) (*RestoreResponse, error) {
	q := url.Values{}
	q.Set("vault", vault)
	path := fmt.Sprintf("/api/engrams/%s/restore?%s", id, q.Encode())

	resp := &RestoreResponse{}
	if err := c.request(ctx, "POST", path, nil, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Traverse traverses the association graph from a starting engram.
// Set followEntities to true to follow entity-level associations in addition to engram-level ones.
func (c *Client) Traverse(ctx context.Context, vault, startID string, maxHops, maxNodes int, relTypes []string, followEntities bool) (*TraverseResponse, error) {
	payload := struct {
		Vault          string   `json:"vault"`
		StartID        string   `json:"start_id"`
		MaxHops        int      `json:"max_hops"`
		MaxNodes       int      `json:"max_nodes"`
		RelTypes       []string `json:"rel_types,omitempty"`
		FollowEntities bool     `json:"follow_entities,omitempty"`
	}{Vault: vault, StartID: startID, MaxHops: maxHops, MaxNodes: maxNodes, RelTypes: relTypes, FollowEntities: followEntities}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	resp := &TraverseResponse{}
	if err := c.request(ctx, "POST", "/api/traverse?"+q.Encode(), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Explain explains why an engram would or wouldn't be returned for a query.
func (c *Client) Explain(ctx context.Context, vault, engramID string, query []string) (*ExplainResponse, error) {
	payload := struct {
		Vault    string   `json:"vault"`
		EngramID string   `json:"engram_id"`
		Query    []string `json:"query"`
	}{Vault: vault, EngramID: engramID, Query: query}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	resp := &ExplainResponse{}
	if err := c.request(ctx, "POST", "/api/explain?"+q.Encode(), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// SetState sets the state of an engram.
func (c *Client) SetState(ctx context.Context, vault, engramID, state, reason string) (*SetStateResponse, error) {
	payload := struct {
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
		Vault  string `json:"vault"`
	}{State: state, Reason: reason, Vault: vault}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	q := url.Values{}
	q.Set("vault", vault)

	resp := &SetStateResponse{}
	if err := c.request(ctx, "PUT", fmt.Sprintf("/api/engrams/%s/state?%s", engramID, q.Encode()), body, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// ListDeleted lists soft-deleted engrams that can be restored.
func (c *Client) ListDeleted(ctx context.Context, vault string, limit int) (*ListDeletedResponse, error) {
	q := url.Values{}
	q.Set("vault", vault)
	q.Set("limit", fmt.Sprintf("%d", limit))
	path := fmt.Sprintf("/api/deleted?%s", q.Encode())

	resp := &ListDeletedResponse{}
	if err := c.request(ctx, "GET", path, nil, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// RetryEnrich retries enrichment plugins for an engram.
func (c *Client) RetryEnrich(ctx context.Context, id, vault string) (*RetryEnrichResponse, error) {
	q := url.Values{}
	q.Set("vault", vault)
	path := fmt.Sprintf("/api/engrams/%s/retry-enrich?%s", id, q.Encode())

	resp := &RetryEnrichResponse{}
	if err := c.request(ctx, "POST", path, nil, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Contradictions lists detected contradictions in a vault.
func (c *Client) Contradictions(ctx context.Context, vault string) (*ContradictionsResponse, error) {
	q := url.Values{}
	q.Set("vault", vault)
	path := fmt.Sprintf("/api/contradictions?%s", q.Encode())

	resp := &ContradictionsResponse{}
	if err := c.request(ctx, "GET", path, nil, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// Guide returns a natural-language guide/summary of a vault's contents.
func (c *Client) Guide(ctx context.Context, vault string) (string, error) {
	q := url.Values{}
	q.Set("vault", vault)
	path := fmt.Sprintf("/api/guide?%s", q.Encode())

	resp := &GuideResponse{}
	if err := c.request(ctx, "GET", path, nil, resp); err != nil {
		return "", err
	}

	return resp.Guide, nil
}

// ListEngrams lists engrams with pagination.
func (c *Client) ListEngrams(ctx context.Context, vault string, limit, offset int) (*ListEngramsResponse, error) {
	q := url.Values{}
	q.Set("vault", vault)
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("offset", fmt.Sprintf("%d", offset))
	path := fmt.Sprintf("/api/engrams?%s", q.Encode())

	resp := &ListEngramsResponse{}
	if err := c.request(ctx, "GET", path, nil, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// GetLinks gets associations/links for an engram.
func (c *Client) GetLinks(ctx context.Context, id, vault string) ([]AssociationItem, error) {
	q := url.Values{}
	q.Set("vault", vault)
	path := fmt.Sprintf("/api/engrams/%s/links?%s", id, q.Encode())

	var resp []AssociationItem
	if err := c.request(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// ListVaults lists all available vaults.
func (c *Client) ListVaults(ctx context.Context) ([]string, error) {
	resp := struct {
		Vaults []string `json:"vaults"`
	}{}
	if err := c.request(ctx, "GET", "/api/vaults", nil, &resp); err != nil {
		return nil, err
	}

	return resp.Vaults, nil
}

// Session gets session activity for a vault.
func (c *Client) Session(ctx context.Context, vault, since string, limit, offset int) (*SessionResponse, error) {
	q := url.Values{}
	q.Set("vault", vault)
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("offset", fmt.Sprintf("%d", offset))
	if since != "" {
		q.Set("since", since)
	}
	path := fmt.Sprintf("/api/session?%s", q.Encode())

	resp := &SessionResponse{}
	if err := c.request(ctx, "GET", path, nil, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// request makes an HTTP request with automatic retry logic.
func (c *Client) request(ctx context.Context, method, path string, body []byte, result interface{}) error {
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		// Create request
		url := c.baseURL + path
		var req *http.Request
		var err error

		if body != nil {
			req, err = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		} else {
			req, err = http.NewRequestWithContext(ctx, method, url, nil)
		}

		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		c.addHeaders(req)

		// Send request
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < c.maxRetries && c.isRetryable(err) {
				c.backoff(attempt)
				continue
			}
			return fmt.Errorf("request failed: %w", err)
		}

		// Handle response
		defer resp.Body.Close()

		if resp.StatusCode >= 500 && attempt < c.maxRetries {
			// Retry on 5xx errors
			lastErr = fmt.Errorf("server error %d", resp.StatusCode)
			c.backoff(attempt)
			continue
		}

		// Check for errors
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(respBody))
		}

		// Parse response
		if result != nil {
			if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
				return fmt.Errorf("failed to decode response: %w", err)
			}
		}

		return nil
	}

	if lastErr != nil {
		return fmt.Errorf("max retries exceeded: %w", lastErr)
	}
	return fmt.Errorf("max retries exceeded")
}

// isRetryable checks if an error is retryable.
func (c *Client) isRetryable(err error) bool {
	// Network errors are retryable
	return strings.Contains(err.Error(), "connection") ||
		strings.Contains(err.Error(), "timeout") ||
		strings.Contains(err.Error(), "temporary failure")
}

// backoff waits with exponential backoff + jitter.
func (c *Client) backoff(attempt int) {
	if attempt > 10 {
		attempt = 10 // Cap exponent to avoid huge waits
	}
	delay := time.Duration(math.Pow(2, float64(attempt))) * c.retryBackoff
	jitter := time.Duration(rand.Intn(100)) * time.Millisecond
	time.Sleep(delay + jitter)
}

// addHeaders adds default headers to the request.
func (c *Client) addHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
	}
}
