package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/skylarbpayne/msgcli/internal/auth"
)

const (
	baseURL = "https://graph.microsoft.com/v1.0"

	// maxPageSize is the largest $top Graph accepts per page when listing
	// messages; larger values are silently capped by the API.
	maxPageSize = 1000
	// maxSearchPageSize is a conservative per-page $top for $search requests.
	maxSearchPageSize = 250
)

// Client is a Microsoft Graph API client
type Client struct {
	httpClient *http.Client
	account    string
	// base is the mailbox path prefix: "/me" with delegated tokens,
	// "/users/{mailbox}" with app-only tokens (which have no /me).
	base    string
	baseURL string
	token   func(ctx context.Context) (string, error)
}

// NewClient creates a new Graph API client for the specified account.
// In app-only mode the account is the target mailbox address.
func NewClient(account string) *Client {
	base := "/me"
	if auth.AppAuthEnabled() {
		base = "/users/" + url.PathEscape(account)
	}
	return &Client{
		httpClient: &http.Client{},
		account:    account,
		base:       base,
		baseURL:    baseURL,
		token: func(ctx context.Context) (string, error) {
			return auth.GetValidToken(ctx, account)
		},
	}
}

// GraphError represents an error response from the Graph API
type GraphError struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		InnerError struct {
			RequestID string `json:"request-id"`
			Date      string `json:"date"`
		} `json:"innerError"`
	} `json:"error"`
}

func (e *GraphError) String() string {
	return fmt.Sprintf("%s: %s", e.Error.Code, e.Error.Message)
}

// request makes an authenticated request to the Graph API and decodes the
// JSON response into result.
func (c *Client) request(ctx context.Context, method, path string, body any, result any) error {
	respBody, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}
	}

	return nil
}

// do makes an authenticated request to the Graph API and returns the raw
// response body. The path may be either a path relative to the API base or an
// absolute URL (as found in @odata.nextLink).
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth error: %w", err)
	}

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	reqURL := c.baseURL + path
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		reqURL = path
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read the response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Check for errors
	if resp.StatusCode >= 400 {
		var graphErr GraphError
		if err := json.Unmarshal(respBody, &graphErr); err == nil && graphErr.Error.Code != "" {
			return nil, fmt.Errorf("Graph API error: %s", graphErr.String())
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// GetRaw performs a GET and returns the raw response body, for non-JSON
// endpoints such as an attachment's /$value content.
func (c *Client) GetRaw(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, "GET", path, nil)
}

// Get performs a GET request
func (c *Client) Get(ctx context.Context, path string, result any) error {
	return c.request(ctx, "GET", path, nil, result)
}

// Post performs a POST request
func (c *Client) Post(ctx context.Context, path string, body any, result any) error {
	return c.request(ctx, "POST", path, body, result)
}

// Patch performs a PATCH request
func (c *Client) Patch(ctx context.Context, path string, body any, result any) error {
	return c.request(ctx, "PATCH", path, body, result)
}

// Delete performs a DELETE request
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.request(ctx, "DELETE", path, nil, nil)
}

// ListResponse is a generic paginated response from Graph API
type ListResponse[T any] struct {
	Value    []T    `json:"value"`
	NextLink string `json:"@odata.nextLink,omitempty"`
	Count    int    `json:"@odata.count,omitempty"`
}

// GetPaged performs a GET and follows @odata.nextLink until limit items have
// been collected or no pages remain. A limit <= 0 fetches a single page.
func GetPaged[T any](ctx context.Context, c *Client, path string, limit int) (*ListResponse[T], error) {
	var all ListResponse[T]
	next := path
	for {
		var page ListResponse[T]
		if err := c.Get(ctx, next, &page); err != nil {
			return nil, err
		}
		all.Value = append(all.Value, page.Value...)
		all.Count = page.Count

		if limit <= 0 {
			return &all, nil
		}
		if len(all.Value) >= limit {
			all.Value = all.Value[:limit]
			return &all, nil
		}
		if page.NextLink == "" || len(page.Value) == 0 {
			return &all, nil
		}
		next = page.NextLink
	}
}

// QueryParams helps build OData query parameters
type QueryParams struct {
	Select  []string
	Filter  string
	OrderBy string
	Top     int
	Skip    int
	Search  string
}

// ToQuery converts QueryParams to a URL query string
func (q *QueryParams) ToQuery() string {
	params := url.Values{}

	if len(q.Select) > 0 {
		params.Set("$select", strings.Join(q.Select, ","))
	}
	if q.Filter != "" {
		params.Set("$filter", q.Filter)
	}
	if q.OrderBy != "" {
		params.Set("$orderby", q.OrderBy)
	}
	if q.Top > 0 {
		params.Set("$top", fmt.Sprintf("%d", q.Top))
	}
	if q.Skip > 0 {
		params.Set("$skip", fmt.Sprintf("%d", q.Skip))
	}
	if q.Search != "" {
		params.Set("$search", fmt.Sprintf("\"%s\"", q.Search))
	}

	encoded := params.Encode()
	if encoded != "" {
		return "?" + encoded
	}
	return ""
}
