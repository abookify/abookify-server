package server

import (
	"crypto/x509"
	"testing"
)

func TestEnsureSelfSignedCert_StableAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	c1, err := EnsureSelfSignedCert(dir, []string{"abc.abookify.e2e.nullbore.com"})
	if err != nil {
		t.Fatal(err)
	}
	f1, err := SPKIFingerprint(c1)
	if err != nil || len(f1) != 44 { // base64 of 32 bytes
		t.Fatalf("fingerprint %q err %v", f1, err)
	}
	c2, err := EnsureSelfSignedCert(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	f2, _ := SPKIFingerprint(c2)
	if f1 != f2 {
		t.Fatalf("a second load must return the same key: %s != %s", f1, f2)
	}
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	if err := leaf.VerifyHostname("abc.abookify.e2e.nullbore.com"); err != nil {
		t.Errorf("SAN missing: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("localhost SAN missing: %v", err)
	}
}
