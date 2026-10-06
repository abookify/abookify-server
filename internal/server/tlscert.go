package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A self-signed TLS identity for this install.
//
// The relay's end-to-end mode forwards the phone's TLS bytes untouched, so the
// server itself has to speak TLS — and the private key must exist ONLY here.
// A public CA cannot issue for a hostname we do not control, so the identity
// is self-signed and the phone pins its SPKI SHA-256 from the pairing QR. That
// pin is the whole proof: a handshake that completes against this key cannot
// have been terminated by anyone in between.
//
// Generated once into <dataDir>/tls/, reused for the life of the install.
// Rotating it means re-pairing every device (by design — the pin IS the trust).

// EnsureSelfSignedCert loads the install's TLS identity from dir, generating a
// P-256 key + 10-year certificate on first use. hosts become SANs (plus
// localhost / 127.0.0.1) so a browser that already trusts the cert matches it;
// the app ignores SANs and pins the key.
func EnsureSelfSignedCert(dir string, hosts []string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "abookify", Organization: []string{"abookify self-hosted server"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// SPKIFingerprint returns base64(sha256(SubjectPublicKeyInfo DER)) of the leaf —
// the value curl's --pinnedpubkey "sha256//…" and the phone's pin compare
// against. It identifies the KEY, not the certificate, so re-issuing a cert
// for the same key keeps every pin valid.
func SPKIFingerprint(c tls.Certificate) (string, error) {
	if len(c.Certificate) == 0 {
		return "", fmt.Errorf("empty certificate")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// certReloader serves the current on-disk identity and re-reads it when the
// files change — so a renewed certificate (Let's Encrypt, every ~60 days)
// goes live without a restart. The KEY stays the same across renewals (renew
// with --reuse-key): the pin IS the key, and a renewal must never make a
// paired phone re-pair. Only a deliberate rotation changes the pin.
type certReloader struct {
	certPath, keyPath string
	mu                sync.Mutex
	cert              *tls.Certificate
	certMod, keyMod   time.Time
	last              time.Time
}

func newCertReloader(certPath, keyPath string, initial tls.Certificate) *certReloader {
	r := &certReloader{certPath: certPath, keyPath: keyPath, cert: &initial}
	r.certMod, r.keyMod = mtime(certPath), mtime(keyPath)
	return r
}

func mtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// get returns the current certificate, checking the files at most every 10 s.
func (r *certReloader) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.last) < 10*time.Second {
		return r.cert, nil
	}
	r.last = time.Now()
	cm, km := mtime(r.certPath), mtime(r.keyPath)
	if cm.Equal(r.certMod) && km.Equal(r.keyMod) {
		return r.cert, nil
	}
	if c, err := tls.LoadX509KeyPair(r.certPath, r.keyPath); err == nil {
		r.cert, r.certMod, r.keyMod = &c, cm, km
	}
	// A half-written or bad pair keeps serving the last good one.
	return r.cert, nil
}
