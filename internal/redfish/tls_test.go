package redfish

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bmc.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost", "bmc.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	fp := CertFingerprint(cert)
	tlsCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return tlsCert, fp
}

func TestNormalizeFingerprint(t *testing.T) {
	want := strings.Repeat("ab", 32)
	cases := []string{
		want,
		strings.ToUpper(want),
		"sha256:" + want,
		"SHA256:" + want,
		FormatFingerprint(want),
	}
	for _, c := range cases {
		got, err := NormalizeFingerprint(c)
		if err != nil || got != want {
			t.Fatalf("%q -> %q %v", c, got, err)
		}
	}
	for _, bad := range []string{"", "deadbeef", "sha256:gg" + strings.Repeat("0", 62), strings.Repeat("0", 63)} {
		if _, err := NormalizeFingerprint(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestTLSFingerprintPin(t *testing.T) {
	cert, fp := testCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/","Name":"root"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}} //nolint:gosec
	srv.StartTLS()
	defer srv.Close()

	ctx := context.Background()
	ok, err := New(Options{BaseURL: srv.URL, TLSFingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ok.Get(ctx, "/redfish/v1/"); err != nil {
		t.Fatalf("matching pin: %v", err)
	}

	// colon form and sha256: prefix
	ok2, err := New(Options{BaseURL: srv.URL, TLSFingerprint: "sha256:" + FormatFingerprint(fp)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ok2.Get(ctx, "/redfish/v1/"); err != nil {
		t.Fatalf("colon pin: %v", err)
	}

	wrong := strings.Repeat("00", 32)
	bad, err := New(Options{BaseURL: srv.URL, TLSFingerprint: wrong})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bad.Get(ctx, "/redfish/v1/")
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("mismatch should fail closed: %v", err)
	}
	if !strings.Contains(err.Error(), FormatFingerprint(fp)) {
		t.Fatalf("error should show peer fingerprint: %v", err)
	}

	// Wrong pin must not fall through even if Insecure is true.
	bad2, err := New(Options{BaseURL: srv.URL, TLSFingerprint: wrong, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad2.Get(ctx, "/redfish/v1/"); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("pin must win over insecure: %v", err)
	}
}

func TestInsecureAndCAFileStillWork(t *testing.T) {
	cert, fp := testCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}} //nolint:gosec
	srv.StartTLS()
	defer srv.Close()
	ctx := context.Background()

	insec, err := New(Options{BaseURL: srv.URL, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insec.Get(ctx, "/redfish/v1/"); err != nil {
		t.Fatalf("insecure: %v", err)
	}

	dir := t.TempDir()
	pemPath := filepath.Join(dir, "ca.pem")
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	block := &pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	withCA, err := New(Options{BaseURL: srv.URL, CAFile: pemPath, Insecure: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withCA.Get(ctx, "/redfish/v1/"); err != nil {
		t.Fatalf("ca_file: %v", err)
	}

	// FetchLeafFingerprint probe
	got, err := FetchLeafFingerprint(ctx, srv.URL, 5*time.Second)
	if err != nil || got != fp {
		t.Fatalf("fetch: %v %s want %s", err, got, fp)
	}
	sum := sha256.Sum256(leaf.Raw)
	if got != hex.EncodeToString(sum[:]) {
		t.Fatal("fingerprint != sha256(raw)")
	}
}

func TestNewRejectsBadFingerprint(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://10.0.0.1", TLSFingerprint: "nope"}); err == nil {
		t.Fatal("bad fingerprint accepted")
	}
}
