package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCredentialsAPI(t *testing.T) {
	// Stub the capability probe so the test never hits the network.
	orig := probeCredentialFn
	probeCredentialFn = func(ProviderDescriptor, map[string]string) []string { return nil }
	defer func() { probeCredentialFn = orig }()

	srv, store, _ := newTestServer(t)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/credentials", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.handleSaveCredential(rec, req)
		return rec
	}

	// Save an OpenAI key.
	if rec := post(`{"provider":"openai","fields":{"api_key":"sk-proj-SECRETmiddle1234"}}`); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}

	// GET returns the vendor catalog (with policy URLs) + the saved key MASKED.
	req := httptest.NewRequest("GET", "/api/credentials", nil)
	rec := httptest.NewRecorder()
	srv.handleListCredentials(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var resp struct {
		Providers   []ProviderDescriptor `json:"providers"`
		Credentials []struct {
			ID       int64             `json:"id"`
			Provider string            `json:"provider"`
			Fields   map[string]string `json:"fields"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Providers) < 3 {
		t.Fatalf("catalog too small: %d", len(resp.Providers))
	}
	policyOK := false
	for _, p := range resp.Providers {
		if p.ID == "openai" && strings.HasPrefix(p.PolicyURL, "https://") {
			policyOK = true
		}
	}
	if !policyOK {
		t.Fatal("openai policy_url missing from catalog")
	}
	if len(resp.Credentials) != 1 {
		t.Fatalf("expected 1 saved credential, got %d", len(resp.Credentials))
	}
	masked := resp.Credentials[0].Fields["api_key"]
	if !strings.Contains(masked, "…") || strings.Contains(masked, "SECRETmiddle") {
		t.Fatalf("api_key not masked on read: %q", masked)
	}

	// The raw key is stored server-side (masking is read-only).
	creds, _ := store.ListCredentials()
	if creds[0].Fields["api_key"] != "sk-proj-SECRETmiddle1234" {
		t.Fatalf("raw key not stored: %q", creds[0].Fields["api_key"])
	}

	// Re-saving the MASKED value (as the UI echoes it) must KEEP the stored key,
	// not overwrite it with the mask.
	if rec := post(`{"provider":"openai","fields":{"api_key":"` + masked + `"}}`); rec.Code != http.StatusOK {
		t.Fatalf("resave masked: %d %s", rec.Code, rec.Body.String())
	}
	creds, _ = store.ListCredentials()
	if creds[0].Fields["api_key"] != "sk-proj-SECRETmiddle1234" {
		t.Fatalf("masked re-save clobbered the key: %q", creds[0].Fields["api_key"])
	}

	// Unknown vendor → 400.
	if rec := post(`{"provider":"nope","fields":{"api_key":"x"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider should 400, got %d", rec.Code)
	}
	// Missing required field on a NEW vendor → 400.
	if rec := post(`{"provider":"google","fields":{}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing required should 400, got %d", rec.Code)
	}

	// Delete.
	id := creds[0].ID
	req = httptest.NewRequest("DELETE", "/api/credentials/"+strconv.FormatInt(id, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec = httptest.NewRecorder()
	srv.handleDeleteCredential(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if creds, _ := store.ListCredentials(); len(creds) != 0 {
		t.Fatalf("not deleted: %d remain", len(creds))
	}
}
