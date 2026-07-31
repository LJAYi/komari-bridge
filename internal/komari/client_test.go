package komari

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/model"
)

func TestRegisterAndReport(t *testing.T) {
	t.Parallel()
	methods := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/clients/register":
			if r.Header.Get("Authorization") != "Bearer discovery-secret" {
				t.Errorf("unexpected Authorization header")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status": "success", "data": map[string]string{"uuid": "u-1", "token": "t-1"},
			})
		case "/api/clients/v2/rpc":
			if r.URL.Query().Get("token") != "t-1" {
				t.Errorf("unexpected token")
			}
			var request struct {
				Method string `json:"method"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			methods <- request.Method
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, "discovery-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	uuid, token, err := client.Register(context.Background(), "node-a")
	if err != nil || uuid != "u-1" || token != "t-1" {
		t.Fatalf("Register() = %q, %q, %v", uuid, token, err)
	}
	if err := client.UploadBasicInfo(context.Background(), token, model.BasicInfo{OS: "PVE"}); err != nil {
		t.Fatal(err)
	}
	if err := client.Report(context.Background(), token, model.Report{}); err != nil {
		t.Fatal(err)
	}
	if got := <-methods; got != "agent.basicInfo" {
		t.Fatalf("first method = %q", got)
	}
	if got := <-methods; got != "agent.report" {
		t.Fatalf("second method = %q", got)
	}
}
