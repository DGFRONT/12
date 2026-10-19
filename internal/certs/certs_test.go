package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, cn string, mod time.Time) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	cert := filepath.Join(dir, "fullchain.pem")
	priv := filepath.Join(dir, "privkey.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	os.Chtimes(cert, mod, mod)
	os.Chtimes(priv, mod, mod)
}

func commonName(t *testing.T, r *Reloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func TestReloadsRenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	writePair(t, dir, "old", base)
	r, err := New(filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if cn := commonName(t, r); cn != "old" {
		t.Fatalf("got %q", cn)
	}

	writePair(t, dir, "new", base.Add(time.Minute))
	if cn := commonName(t, r); cn != "new" {
		t.Fatalf("renewed certificate not picked up, got %q", cn)
	}

	// A broken renewal keeps the last good certificate.
	os.WriteFile(filepath.Join(dir, "privkey.pem"), []byte("garbage"), 0o600)
	os.Chtimes(filepath.Join(dir, "privkey.pem"), base.Add(2*time.Minute), base.Add(2*time.Minute))
	if cn := commonName(t, r); cn != "new" {
		t.Fatalf("broken renewal replaced the certificate, got %q", cn)
	}
}

func TestMissingFiles(t *testing.T) {
	if _, err := New("/nonexistent/cert.pem", "/nonexistent/key.pem", time.Minute); err == nil {
		t.Fatal("expected error")
	}
}
