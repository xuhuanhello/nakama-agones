package gamefleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var fixtureTLS TLSFiles
var fixtureCertificate tls.Certificate

func TestMain(m *testing.M) {
	d, e := os.MkdirTemp("", "gamefleet-tls-test-")
	if e != nil {
		panic(e)
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		panic(e)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, DNSNames: []string{"localhost"}}
	der, e := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if e != nil {
		panic(e)
	}
	raw, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		panic(e)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	priv := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
	fixtureTLS = TLSFiles{filepath.Join(d, "ca.pem"), filepath.Join(d, "cert.pem"), filepath.Join(d, "key.pem")}
	for p, b := range map[string][]byte{fixtureTLS.CAFile: cert, fixtureTLS.CertFile: cert, fixtureTLS.KeyFile: priv} {
		if e := os.WriteFile(p, b, 0600); e != nil {
			panic(e)
		}
	}
	fixtureCertificate, e = tls.X509KeyPair(cert, priv)
	if e != nil {
		panic(e)
	}
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}
func startBusinessTestServer(s *httptest.Server) {
	ca, _ := os.ReadFile(fixtureTLS.CAFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{fixtureCertificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12}
	s.StartTLS()
}
func newBusinessTestServer(h http.Handler) *httptest.Server {
	s := httptest.NewUnstartedServer(h)
	startBusinessTestServer(s)
	return s
}
