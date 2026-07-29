package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// GET /api/credentials — the vendor catalog (to render the Keys form) plus the
// user's saved credentials with secret fields masked. Everything the Keys UI and
// mobile need to render the section comes from here (single source).
func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	saved, err := s.store.ListCredentials()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	type maskedCred struct {
		ID           int64             `json:"id"`
		Provider     string            `json:"provider"`
		Label        string            `json:"label"`
		Fields       map[string]string `json:"fields"`
		Capabilities []string          `json:"capabilities"`
	}
	out := make([]maskedCred, 0, len(saved))
	for _, c := range saved {
		desc, _ := providerDescriptor(c.Provider)
		mf := map[string]string{}
		for k, v := range c.Fields {
			if v != "" && fieldIsSecret(desc, k) {
				mf[k] = maskSecret(v)
			} else {
				mf[k] = v
			}
		}
		out = append(out, maskedCred{ID: c.ID, Provider: c.Provider, Label: c.Label, Fields: mf, Capabilities: c.Capabilities})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":   ProviderCatalog(),
		"credentials": out,
	})
}

// POST /api/credentials — create/replace the one credential for a vendor. A
// blank or already-masked secret field means "keep the stored value" so the user
// only retypes a key to rotate it. An added key that nothing consumes yet is
// fine (the whole point of a standalone Keys section).
func (s *Server) handleSaveCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string            `json:"provider"`
		Label    string            `json:"label"`
		Fields   map[string]string `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	desc, ok := providerDescriptor(body.Provider)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown provider"})
		return
	}

	// Prior fields for this vendor, so blank/masked secrets are preserved.
	var prior map[string]string
	if existing, _ := s.store.ListCredentials(); existing != nil {
		for _, c := range existing {
			if c.Provider == body.Provider {
				prior = c.Fields
				break
			}
		}
	}

	fields := map[string]string{}
	missing := []string{}
	for _, f := range desc.CredentialFields {
		v := strings.TrimSpace(body.Fields[f.Key])
		if f.Secret && (v == "" || isMaskedSecret(v)) {
			if prior != nil {
				v = prior[f.Key] // keep the stored secret
			} else {
				v = ""
			}
		}
		if f.Required && v == "" {
			missing = append(missing, f.Label)
		}
		fields[f.Key] = v
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing required field(s): " + strings.Join(missing, ", ")})
		return
	}

	id, err := s.store.UpsertProviderCredential(body.Provider, body.Label, fields)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// DELETE /api/credentials/{id}
func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := s.store.DeleteCredential(id); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
