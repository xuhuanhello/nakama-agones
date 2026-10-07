package gamefleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestBusinessTransportMutualTLSAndCertificateFailures(t *testing.T) {
	var requests atomic.Int32
	s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	defer s.Close()
	for _, name := range []string{"valid", "missing-client", "wrong-client", "expired-client", "untrusted-server", "wrong-hostname"} {
		t.Run(name, func(t *testing.T) {
			base, tr, e := businessTransport(s.URL, fixtureTLS)
			if e != nil {
				t.Fatal(e)
			}
			defer tr.CloseIdleConnections()
			switch name {
			case "missing-client":
				tr.TLSClientConfig.Certificates = nil
			case "untrusted-server":
				tr.TLSClientConfig.RootCAs = x509.NewCertPool()
			case "wrong-hostname":
				tr.TLSClientConfig.ServerName = "wrong.example.test"
			case "wrong-client", "expired-client":
				parent, e := x509.ParseCertificate(fixtureCertificate.Certificate[0])
				if e != nil {
					t.Fatal(e)
				}
				k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if e != nil {
					t.Fatal(e)
				}
				c := &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
				signer := fixtureCertificate.PrivateKey
				if name == "expired-client" {
					c.NotAfter = time.Now().Add(-time.Minute)
				} else {
					parent = c
					signer = k
				}
				der, e := x509.CreateCertificate(rand.Reader, c, parent, &k.PublicKey, signer)
				if e != nil {
					t.Fatal(e)
				}
				tr.TLSClientConfig.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}
			}
			before := requests.Load()
			client := &http.Client{Transport: tr, Timeout: time.Second * 3}
			response, e := client.Get(base)
			if name == "valid" {
				if e != nil {
					t.Fatal(e)
				}
				response.Body.Close()
				if response.StatusCode != 204 {
					t.Fatal(response.StatusCode)
				}
			} else {
				if e == nil {
					response.Body.Close()
					t.Fatal("invalid TLS identity accepted")
				}
				if requests.Load() != before {
					t.Fatal("failed TLS identity reached business handler")
				}
			}
		})
	}
}
func TestTLSFilesAreExplicitPrivateAndReloaded(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:17682", "https://user@business.test", "https://business.test/path", "https://business.test?x=1", "https://business.test:0443"} {
		if _, _, e := businessTransport(raw, fixtureTLS); e == nil {
			t.Fatal("ambiguous/plaintext origin accepted")
		}
	}
	d := t.TempDir()
	key := filepath.Join(d, "key")
	raw, _ := os.ReadFile(fixtureTLS.KeyFile)
	os.WriteFile(key, raw, 0644)
	files := fixtureTLS
	files.KeyFile = key
	if _, _, e := businessTransport("https://business.test", files); e == nil {
		t.Fatal("publicly readable key accepted")
	}
	os.Chmod(key, 0600)
	_, tr, e := businessTransport("https://business.test", files)
	if e != nil {
		t.Fatal(e)
	}
	tr.CloseIdleConnections()
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("invalid rotated material")}), 0600)
	if _, _, e := businessTransport("https://business.test", files); e == nil {
		t.Fatal("restart reused stale key rather than loading rotation")
	}
	if _, _, e := businessTransport("https://business.test", TLSFiles{}); e == nil {
		t.Fatal("missing identity accepted")
	}
}
