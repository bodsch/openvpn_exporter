package tlsfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestCache(t *testing.T) {
	dir := t.TempDir()
	tf, err := WriteTestFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCache()

	if _, err := c.CRL(tf.CRL); err != nil {
		t.Fatal(err)
	}
	certs, err := c.Certificates(tf.Server)
	if err != nil || len(certs) != 1 || certs[0].Subject != "vpn.example.com" {
		t.Fatalf("certs = %+v, err = %v", certs, err)
	}

	// Unchanged metadata is served from the cache even if the content differs.
	data, err := os.ReadFile(tf.CA)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(tf.Server)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tf.Server, data[:fi.Size()], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tf.Server, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if certs, err := c.Certificates(tf.Server); err != nil || certs[0].Subject != "vpn.example.com" {
		t.Errorf("cached certs = %+v, err = %v", certs, err)
	}

	// A changed file is parsed again.
	if err := os.WriteFile(tf.Server, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tf.Server, fi.ModTime().Add(time.Second), fi.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if certs, err := c.Certificates(tf.Server); err != nil || certs[0].Subject != "Test VPN CA" {
		t.Errorf("reloaded certs = %+v, err = %v", certs, err)
	}

	// Errors are not cached, and a vanished file is an error.
	if err := os.Remove(tf.CRL); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CRL(tf.CRL); err == nil {
		t.Error("removed CRL: expected error")
	}
}

func BenchmarkCRL(b *testing.B) {
	tf, err := WriteTestFiles(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Run("uncached", func(b *testing.B) {
		for b.Loop() {
			if _, err := LoadCRL(tf.CRL); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		c := NewCache()
		for b.Loop() {
			if _, err := c.CRL(tf.CRL); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestLoadTooLarge(t *testing.T) {
	big := filepath.Join(t.TempDir(), "big.pem")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileSize + 1); err != nil { // sparse, costs no disk space
		t.Fatal(err)
	}
	f.Close()
	if _, err := LoadCRL(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("LoadCRL: err = %v", err)
	}
	if _, err := NewCache().Certificates(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("Cache.Certificates: err = %v", err)
	}
}
