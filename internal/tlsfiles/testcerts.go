package tlsfiles

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
	"time"
)

// TestFiles are PEM files written by WriteTestFiles.
type TestFiles struct {
	CA, Server, Inline, CRL string
	CANotAfter              time.Time
	ServerNotAfter          time.Time
	CRLNextUpdate           time.Time
}

// WriteTestFiles creates a CA, a server certificate, an OpenVPN config with
// inline certificates and a CRL revoking one certificate in dir. It is meant
// for tests only.
func WriteTestFiles(dir string) (*TestFiles, error) {
	now := time.Now().Truncate(time.Second)
	tf := &TestFiles{
		CA:             filepath.Join(dir, "ca.crt"),
		Server:         filepath.Join(dir, "server.crt"),
		Inline:         filepath.Join(dir, "server.conf"),
		CRL:            filepath.Join(dir, "crl.pem"),
		CANotAfter:     now.Add(10 * 365 * 24 * time.Hour),
		ServerNotAfter: now.Add(20 * 24 * time.Hour),
		CRLNextUpdate:  now.Add(7 * 24 * time.Hour),
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test VPN CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              tf.CANotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(0x2A),
		Subject:      pkix.Name{CommonName: "vpn.example.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     tf.ServerNotAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	crlDER, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: now.Add(-time.Hour),
		NextUpdate: tf.CRLNextUpdate,
		RevokedCertificateEntries: []x509.RevocationListEntry{
			{SerialNumber: big.NewInt(99), RevocationTime: now.Add(-time.Minute)},
		},
	}, caCert, caKey)
	if err != nil {
		return nil, err
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	srvPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	inline := "port 1194\nproto udp\n<ca>\n" + string(caPEM) + "</ca>\n<cert>\n" + string(srvPEM) + "</cert>\n"

	for path, data := range map[string][]byte{
		tf.CA:     caPEM,
		tf.Server: srvPEM,
		tf.Inline: []byte(inline),
		tf.CRL:    pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: crlDER}),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return nil, err
		}
	}
	return tf, nil
}
