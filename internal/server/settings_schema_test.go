package server

import "testing"

// The schema is the source of truth web + mobile render from, so guard its
// invariants: unique keys, valid types, and secret flags consistent with the
// server's actual masking (isSecretSettingKey) so a "secret" field is really
// masked on the wire.
func TestSettingsSchemaConsistency(t *testing.T) {
	validTypes := map[string]bool{"text": true, "secret": true, "bool": true, "select": true, "select_or_custom": true}
	seen := map[string]bool{}
	doc := SettingsSchema()
	if doc.Version != SettingsSchemaVersion {
		t.Errorf("version = %d, want %d", doc.Version, SettingsSchemaVersion)
	}
	if len(doc.Groups) == 0 {
		t.Fatal("schema has no groups")
	}
	for _, g := range doc.Groups {
		if g.Key == "" || g.Title == "" {
			t.Errorf("group missing key/title: %+v", g)
		}
		for _, f := range g.Fields {
			if f.Key == "" {
				t.Errorf("field in group %q missing key", g.Key)
			}
			if seen[f.Key] {
				t.Errorf("duplicate field key %q", f.Key)
			}
			seen[f.Key] = true
			if !validTypes[f.Type] {
				t.Errorf("field %q has invalid type %q", f.Key, f.Type)
			}
			// A field marked secret must actually be masked by the server, and
			// vice-versa — otherwise the UI promises masking the wire doesn't do.
			if f.Secret != isSecretSettingKey(f.Key) {
				t.Errorf("field %q Secret=%v but isSecretSettingKey=%v", f.Key, f.Secret, isSecretSettingKey(f.Key))
			}
			if f.Type == "select" && len(f.Options) == 0 && len(f.OptionGroups) == 0 {
				t.Errorf("select field %q has no options", f.Key)
			}
			if f.Type == "select_or_custom" && f.OptionsEndpoint == "" {
				t.Errorf("select_or_custom field %q has no options_endpoint", f.Key)
			}
		}
	}
	// The write-only password must never be returned by GET (auth_password is
	// stored as auth_password_hash; the raw field is write-only).
	for _, g := range doc.Groups {
		for _, f := range g.Fields {
			if f.Key == "auth_password" && !f.WriteOnly {
				t.Error("auth_password must be write_only")
			}
		}
	}
}
