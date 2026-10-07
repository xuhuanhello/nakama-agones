package gamefleet

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// TLSFiles is explicit per-endpoint trust and machine identity, independent of gfsvc scope.
// Rotate files atomically and restart the runtime to load the new identity.
type TLSFiles struct{ CAFile, CertFile, KeyFile string }

func tlsFilesFromEnv(prefix string) TLSFiles {
	return TLSFiles{os.Getenv(prefix + "_CA_FILE"), os.Getenv(prefix + "_CERT_FILE"), os.Getenv(prefix + "_TLS_KEY_FILE")}
}
func readTLSFile(path string) ([]byte, error) {
	i, e := os.Lstat(path)
	if e != nil || !privateServiceKeyFile(i) || i.Size() > 1<<20 {
		return nil, errors.New("TLS material requires private regular files (0400 or 0600)")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("cannot read TLS material")
	}
	defer f.Close()
	opened, e := f.Stat()
	current, pe := os.Lstat(path)
	if e != nil || pe != nil || !privateServiceKeyFile(opened) || !privateServiceKeyFile(current) || !os.SameFile(i, opened) || !os.SameFile(current, opened) {
		return nil, errors.New("TLS material changed during read")
	}
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, errors.New("cannot read TLS material")
	}
	return b, nil
}
func businessTransport(raw string, files TLSFiles) (string, *http.Transport, error) {
	u, e := url.Parse(raw)
	if e != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" || strings.Contains(u.Hostname(), "%") {
		return "", nil, errors.New("business endpoint must be an HTTPS origin")
	}
	if u.Port() != "" {
		p, e := strconv.Atoi(u.Port())
		if e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != u.Port() {
			return "", nil, errors.New("invalid HTTPS port")
		}
	}
	ca, e := readTLSFile(files.CAFile)
	if e != nil {
		return "", nil, e
	}
	cert, e := readTLSFile(files.CertFile)
	if e != nil {
		return "", nil, e
	}
	key, e := readTLSFile(files.KeyFile)
	if e != nil {
		return "", nil, e
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return "", nil, errors.New("invalid business CA")
	}
	identity, e := tls.X509KeyPair(cert, key)
	if e != nil {
		return "", nil, errors.New("invalid business client identity")
	}
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{identity}}, TLSHandshakeTimeout: 3 * time.Second,
		MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10, DisableCompression: true}
	return "https://" + u.Host, tr, nil
}
