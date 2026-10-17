package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// TargetTLS is a remote server's TLS verification settings (see
// FileServer's tls-* fields). The zero value verifies the receiver's
// certificate against the system roots and its hostname, Go's default.
// It is comparable, so callers can key cached HTTP clients by it.
type TargetTLS struct {
	// InsecureSkipVerify accepts any certificate the receiver presents.
	InsecureSkipVerify bool
	// SkipHostnameVerify still verifies the certificate chain, but not
	// that it was issued for the endpoint's host.
	SkipHostnameVerify bool
	// CA is PEM-encoded certificates that replace the system roots.
	CA string
}

// IsZero reports whether t is Go's default verification.
func (t TargetTLS) IsZero() bool { return t == TargetTLS{} }

// ClientConfig builds the tls.Config t describes, or nil for the zero value
// (the transport's default). A CA that holds no valid PEM certificate is
// an error; ResolveServer already rejects one.
func (t TargetTLS) ClientConfig() (*tls.Config, error) {
	if t.IsZero() {
		return nil, nil //nolint:nilnil // nil means the transport's default.
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if t.InsecureSkipVerify {
		// security: explicitly configured per server; the dashboard warns.
		cfg.InsecureSkipVerify = true

		return cfg, nil
	}

	var roots *x509.CertPool

	if t.CA != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(t.CA)) {
			return nil, errors.New("tls-ca contains no valid PEM certificate")
		}

		cfg.RootCAs = roots
	}

	if t.SkipHostnameVerify {
		// crypto/tls has no "chain only" mode: skip its verification and
		// verify the chain ourselves in VerifyConnection, without DNSName.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyChain(cs.PeerCertificates, roots)
		}
	}

	return cfg, nil
}

// verifyChain verifies certs (leaf first) against roots (nil: the system
// roots) without checking the leaf's hostname.
func verifyChain(certs []*x509.Certificate, roots *x509.CertPool) error {
	if len(certs) == 0 {
		return errors.New("tls: receiver presented no certificate")
	}

	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}

	_, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}

	return nil
}

// fileServerTLS returns fs's tls-* fields as a TargetTLS.
func fileServerTLS(fs *FileServer) TargetTLS {
	return TargetTLS{
		InsecureSkipVerify: fs.TLSInsecureSkipVerify,
		SkipHostnameVerify: fs.TLSSkipHostnameVerify,
		CA:                 strings.TrimSpace(fs.TLSCA),
	}
}

// resolveServerTLS validates a remote server's tls-* fields against its
// endpoint: they need an https endpoint, tls-insecure-skip-verify excludes
// the other two, and tls-ca must hold at least one PEM certificate.
func resolveServerTLS(endpoint string, t TargetTLS) (TargetTLS, error) {
	if t.IsZero() {
		return t, nil
	}

	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return TargetTLS{}, errors.New("tls-* settings need an https endpoint")
	}

	if t.InsecureSkipVerify && (t.SkipHostnameVerify || t.CA != "") {
		return TargetTLS{}, errors.New("tls-insecure-skip-verify skips all verification; it can't be combined with tls-skip-hostname-verify or tls-ca")
	}

	if _, err := t.ClientConfig(); err != nil {
		return TargetTLS{}, err
	}

	return t, nil
}
