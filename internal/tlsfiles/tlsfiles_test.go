package tlsfiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	tf, err := WriteTestFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	certs, err := LoadCertificates(tf.Server)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 1 {
		t.Fatalf("got %d certs", len(certs))
	}
	c := certs[0]
	if c.Subject != "vpn.example.com" || c.Issuer != "Test VPN CA" || c.Serial != "2A" || !c.NotAfter.Equal(tf.ServerNotAfter) {
		t.Errorf("cert = %+v", c)
	}

	inline, err := LoadCertificates(tf.Inline)
	if err != nil {
		t.Fatal(err)
	}
	if len(inline) != 2 || inline[0].Subject != "Test VPN CA" || inline[1].Subject != "vpn.example.com" {
		t.Errorf("inline certs = %+v", inline)
	}

	crl, err := LoadCRL(tf.CRL)
	if err != nil {
		t.Fatal(err)
	}
	if crl.Issuer != "Test VPN CA" || crl.Revoked != 1 || !crl.NextUpdate.Equal(tf.CRLNextUpdate) {
		t.Errorf("crl = %+v", crl)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertificates(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file: expected error")
	}
	if _, err := LoadCertificates(garbage); err == nil {
		t.Error("garbage cert: expected error")
	}
	if _, err := LoadCRL(garbage); err == nil {
		t.Error("garbage CRL: expected error")
	}
}
