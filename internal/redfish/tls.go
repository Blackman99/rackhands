package redfish

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// NormalizeFingerprint accepts a SHA-256 fingerprint as bare hex, colon-hex,
// or with a "sha256:" prefix. Returns lowercase hex without separators.
func NormalizeFingerprint(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("tls_fingerprint is empty")
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "sha256:") {
		s = s[len("sha256:"):]
	}
	var b strings.Builder
	b.Grow(64)
	for _, r := range s {
		switch {
		case r == ':' || r == ' ' || r == '-':
			continue
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'f':
			b.WriteRune(r)
		case r >= 'A' && r <= 'F':
			b.WriteRune(r + ('a' - 'A'))
		default:
			return "", fmt.Errorf("tls_fingerprint: invalid character %q (want SHA-256 hex, optionally colon-separated or sha256: prefix)", r)
		}
	}
	out := b.String()
	if len(out) != 64 {
		return "", fmt.Errorf("tls_fingerprint: want 64 hex characters (SHA-256 of leaf cert), got %d", len(out))
	}
	return out, nil
}

// CertFingerprint is the lowercase hex SHA-256 of cert.Raw (DER).
func CertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// FormatFingerprint inserts colons every two hex digits for display.
func FormatFingerprint(hex64 string) string {
	hex64 = strings.ToLower(strings.ReplaceAll(hex64, ":", ""))
	if len(hex64) != 64 {
		return hex64
	}
	var b strings.Builder
	b.Grow(95)
	for i := 0; i < 64; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hex64[i : i+2])
	}
	return b.String()
}

func pinVerifier(want string) func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tls: peer presented no certificate")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("tls: parse peer certificate: %w", err)
		}
		got := CertFingerprint(cert)
		if got != want {
			return fmt.Errorf("tls: certificate fingerprint mismatch: expected %s, got %s",
				FormatFingerprint(want), FormatFingerprint(got))
		}
		return nil
	}
}

// FetchLeafFingerprint dials address over TLS (without trusting CAs) and
// returns the SHA-256 fingerprint of the leaf certificate. Read-only probe
// for operators to paste into tls_fingerprint:.
func FetchLeafFingerprint(ctx context.Context, address string, timeout time.Duration) (string, error) {
	raw := address
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad address %q", address)
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	serverName, _, _ := net.SplitHostPort(host)
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // probe only; operator pins the result
		ServerName:         serverName,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer tlsConn.Close()
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("tls: peer presented no certificate")
	}
	return CertFingerprint(state.PeerCertificates[0]), nil
}
