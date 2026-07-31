package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LJAYi/komari-bridge/internal/slurm"
)

func TestSlurmAPIAuthenticationAndLookup(t *testing.T) {
	t.Parallel()
	store := slurm.NewStore()
	store.Set("gpu-host", slurm.Snapshot{SourceID: "gpu-host", CollectedAt: time.Now(), JobsRunning: 3})
	server := httptest.NewServer(New(store, "secret").Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/slurm/gpu-host")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/slurm/gpu-host", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d", resp.StatusCode)
	}
}
