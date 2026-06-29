package server

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// A client must not be able to write auth_password_hash directly through the
// settings body — that would let anyone on an open server install a chosen
// bcrypt digest, enable auth, and lock out the owner. The only legitimate way
// to set the hash is the bcrypt-from-auth_password path.
func TestSaveSettingsRejectsDirectPasswordHash(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// Attacker tries to plant a known hash directly.
	known, _ := bcrypt.GenerateFromPassword([]byte("attacker-knows-this"), bcrypt.DefaultCost)
	postSettings(t, srv, `{"auth_password_hash":"`+string(known)+`"}`)

	if h, _ := store.GetSetting("auth_password_hash"); h != "" {
		t.Fatalf("auth_password_hash was written directly (%q) — settings body must not set it", h)
	}
	if srv.authEnabled() {
		t.Error("auth got enabled via a direct hash write — owner could be locked out")
	}

	// server_install_id is likewise reserved (rotated via its own endpoint).
	before, _ := store.GetSetting("server_install_id")
	postSettings(t, srv, `{"server_install_id":"hijacked-slug"}`)
	if after, _ := store.GetSetting("server_install_id"); after == "hijacked-slug" {
		t.Errorf("server_install_id writable via settings body (%q)", after)
	}
	_ = before
}

// The legitimate path still works: auth_password is hashed into
// auth_password_hash, enabling auth, and a normal setting saves alongside.
func TestSaveSettingsAuthPasswordPathStillWorks(t *testing.T) {
	srv, store, _ := newTestServer(t)
	postSettings(t, srv, `{"auth_password":"correct horse","auth_username":"pj","tts_voice":"af_bella"}`)

	if !srv.authEnabled() {
		t.Fatal("auth_password did not enable auth")
	}
	hash, _ := store.GetSetting("auth_password_hash")
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse")) != nil {
		t.Error("stored hash does not verify against the supplied password")
	}
	// The bare plaintext key is never persisted.
	if pw, _ := store.GetSetting("auth_password"); pw != "" {
		t.Errorf("auth_password plaintext persisted (%q)", pw)
	}
	if v, _ := store.GetSetting("tts_voice"); v != "af_bella" {
		t.Errorf("co-saved normal setting = %q, want af_bella", v)
	}

	// Empty password clears the hash → disables auth (back to open server).
	postSettings(t, srv, `{"auth_password":""}`)
	if srv.authEnabled() {
		t.Error("empty auth_password should have disabled auth")
	}
}
