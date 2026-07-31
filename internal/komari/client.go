package komari

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/LJAYi/komari-bridge/internal/model"
)

type Client struct {
	endpoint string
	adKey    string
	http     *http.Client
	seq      atomic.Uint64
}

func New(endpoint, autoDiscoveryKey string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid Komari endpoint %q", endpoint)
	}
	return &Client{
		endpoint: u.String(),
		adKey:    autoDiscoveryKey,
		http:     &http.Client{Timeout: timeout},
	}, nil
}

type envelope[T any] struct {
	Status string `json:"status"`
	Data   T      `json:"data"`
	Error  string `json:"error"`
}

type registration struct {
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

func (c *Client) Register(ctx context.Context, name string) (string, string, error) {
	u, _ := url.Parse(c.endpoint + "/api/clients/register")
	q := u.Query()
	q.Set("name", name)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.adKey)
	req.Header.Set("Content-Type", "application/json")

	var response envelope[registration]
	if err := c.doJSON(req, &response); err != nil {
		return "", "", fmt.Errorf("register client: %w", err)
	}
	if response.Status != "success" || response.Data.UUID == "" || response.Data.Token == "" {
		return "", "", fmt.Errorf("register client: %s", firstNonEmpty(response.Error, "unexpected response"))
	}
	return response.Data.UUID, response.Data.Token, nil
}

func (c *Client) UploadBasicInfo(ctx context.Context, token string, info model.BasicInfo) error {
	return c.rpc(ctx, token, "agent.basicInfo", map[string]any{"info": info})
}

func (c *Client) Report(ctx context.Context, token string, report model.Report) error {
	return c.rpc(ctx, token, "agent.report", map[string]any{
		"report": report, "ack_event_ids": []string{},
	})
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) rpc(ctx context.Context, token, method string, params any) error {
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      fmt.Sprintf("bridge-%d", c.seq.Add(1)),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("encode %s: %w", method, err)
	}
	u, _ := url.Parse(c.endpoint + "/api/clients/v2/rpc")
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var response rpcResponse
	if err := c.doJSON(req, &response); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if response.Error != nil {
		return fmt.Errorf("%s: RPC %d: %s", method, response.Error.Code, response.Error.Message)
	}
	return nil
}

func (c *Client) doJSON(req *http.Request, dst any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
