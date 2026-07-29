package db

import "testing"

func TestCredentialsCRUDAndOneToMany(t *testing.T) {
	store := testStore(t)

	// Create + read back with fields intact.
	id, err := store.CreateCredential("openai", "", map[string]string{"api_key": "sk-proj-abc"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetCredential(id)
	if err != nil || got == nil {
		t.Fatalf("get: %v (got %v)", err, got)
	}
	if got.Provider != "openai" || got.Fields["api_key"] != "sk-proj-abc" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Upsert updates the canonical row in place (same id), not a new row.
	id2, err := store.UpsertProviderCredential("openai", "", map[string]string{"api_key": "sk-proj-new"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id2 != id {
		t.Fatalf("upsert created a new row (%d) instead of updating %d", id2, id)
	}
	got, _ = store.GetCredential(id)
	if got.Fields["api_key"] != "sk-proj-new" {
		t.Fatalf("upsert didn't update fields: %+v", got)
	}

	// A provider that declares more than one field (Azure) round-trips whole.
	azureID, err := store.UpsertProviderCredential("azure_openai", "", map[string]string{"api_key": "az", "region": "eastus", "deployment": "gpt4o"})
	if err != nil {
		t.Fatalf("azure upsert: %v", err)
	}
	az, _ := store.GetCredential(azureID)
	if az.Fields["region"] != "eastus" || az.Fields["deployment"] != "gpt4o" {
		t.Fatalf("multi-field creds not stored: %+v", az)
	}

	// Forward-compat: the table PERMITS multiple rows per provider (no UNIQUE),
	// so a future per-lane-cost UI isn't precluded. CreateCredential (not the
	// upsert helper) must be able to add a 2nd openai key.
	if _, err := store.CreateCredential("openai", "cost-lane-B", map[string]string{"api_key": "sk-proj-2"}); err != nil {
		t.Fatalf("second openai credential rejected — one-to-many precluded: %v", err)
	}
	all, _ := store.ListCredentials()
	openaiCount := 0
	for _, c := range all {
		if c.Provider == "openai" {
			openaiCount++
		}
	}
	if openaiCount != 2 {
		t.Fatalf("expected 2 openai rows (one-to-many), got %d", openaiCount)
	}

	// Capabilities: default is empty (nothing verified yet), then a verify pass
	// records exactly what this credential can serve — the UI gates features on
	// this, so a Google key that can't reach Google Cloud TTS won't offer it.
	if len(got.Capabilities) != 0 {
		t.Fatalf("new credential should have no capabilities until verified, got %v", got.Capabilities)
	}
	if err := store.SetCredentialCapabilities(id, []string{"llm", "voice"}); err != nil {
		t.Fatalf("set capabilities: %v", err)
	}
	got, _ = store.GetCredential(id)
	if len(got.Capabilities) != 2 || got.Capabilities[0] != "llm" || got.Capabilities[1] != "voice" {
		t.Fatalf("capabilities roundtrip failed: %v", got.Capabilities)
	}

	// Delete.
	if err := store.DeleteCredential(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if c, _ := store.GetCredential(id); c != nil {
		t.Fatalf("credential not deleted")
	}

	// GetCredential on a missing id → (nil, nil), not an error.
	if c, err := store.GetCredential(99999); err != nil || c != nil {
		t.Fatalf("missing get should be (nil,nil), got (%v,%v)", c, err)
	}
}

func TestCredentialAPIKey(t *testing.T) {
	store := testStore(t)

	// No credential → empty (resolution falls back to legacy/local).
	if k := store.CredentialAPIKey("openai"); k != "" {
		t.Fatalf("expected empty for missing provider, got %q", k)
	}

	if _, err := store.UpsertProviderCredential("openai", "", map[string]string{"api_key": "sk-proj-vault"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if k := store.CredentialAPIKey("openai"); k != "sk-proj-vault" {
		t.Fatalf("expected the vault key back, got %q", k)
	}

	// A second row for the same provider must not shadow the canonical one — the
	// resolver uses the lowest-id row, matching UpsertProviderCredential.
	if _, err := store.CreateCredential("openai", "cost-lane-B", map[string]string{"api_key": "sk-proj-second"}); err != nil {
		t.Fatalf("second create: %v", err)
	}
	if k := store.CredentialAPIKey("openai"); k != "sk-proj-vault" {
		t.Fatalf("resolver should return the canonical (lowest-id) key, got %q", k)
	}
}
