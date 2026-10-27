// Package tlsfiles reads certificates and CRLs used by an OpenVPN server.
package tlsfiles

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"
)

// Certificate holds the expiry relevant fields of an X.509 certificate.
type Certificate struct {
	Subject   string
	Issuer    string
	Serial    string
	NotBefore time.Time
	NotAfter  time.Time
}

// CRL holds the validity relevant fields of a certificate revocation list.
type CRL struct {
	Issuer     string
	ThisUpdate time.Time
	NextUpdate time.Time
	Revoked    int
}

// LoadCertificates returns all certificates of a PEM or DER file. PEM blocks
// are found anywhere in the file, so OpenVPN configs with inline <ca> and
// <cert> sections work as well.
func LoadCertificates(path string) ([]Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var ders [][]byte
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			ders = append(ders, block.Bytes)
		}
	}
	if len(ders) == 0 {
		ders = [][]byte{data}
	}

	certs := make([]Certificate, 0, len(ders))
	for _, der := range ders {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("%s: parsing certificate: %w", path, err)
		}
		certs = append(certs, Certificate{
			Subject:   name(c.Subject.CommonName, c.Subject.String()),
			Issuer:    name(c.Issuer.CommonName, c.Issuer.String()),
			Serial:    fmt.Sprintf("%X", c.SerialNumber),
			NotBefore: c.NotBefore,
			NotAfter:  c.NotAfter,
		})
	}
	return certs, nil
}

// LoadCRL reads a PEM or DER encoded certificate revocation list.
func LoadCRL(path string) (*CRL, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	der := data
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != "X509 CRL" {
			return nil, fmt.Errorf("%s: unexpected PEM block %q", path, block.Type)
		}
		der = block.Bytes
	}
	rl, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, fmt.Errorf("%s: parsing CRL: %w", path, err)
	}
	if rl.NextUpdate.IsZero() {
		return nil, errors.New(path + ": CRL has no nextUpdate")
	}
	return &CRL{
		Issuer:     name(rl.Issuer.CommonName, rl.Issuer.String()),
		ThisUpdate: rl.ThisUpdate,
		NextUpdate: rl.NextUpdate,
		Revoked:    len(rl.RevokedCertificateEntries),
	}, nil
}

func name(cn, full string) string {
	if cn != "" {
		return cn
	}
	return full
}
