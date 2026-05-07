package mitm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

type cursorCertAuthority struct {
	cert  *x509.Certificate
	key   *rsa.PrivateKey
	cache map[string]*tls.Certificate
	mu    sync.Mutex
}

func newCursorCertAuthority() (*cursorCertAuthority, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate cursor mitm ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := currentTime()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Clyde Cursor MITM Ephemeral CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create cursor mitm ca cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse cursor mitm ca cert: %w", err)
	}
	return &cursorCertAuthority{
		cert:  cert,
		key:   key,
		cache: make(map[string]*tls.Certificate),
	}, nil
}

func (ca *cursorCertAuthority) leafForHost(host string) (*tls.Certificate, error) {
	host = strings.Trim(strings.ToLower(host), "[]")
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if cert := ca.cache[host]; cert != nil {
		return cert, nil
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate cursor mitm leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := currentTime()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(12 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
		template.DNSNames = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("create cursor mitm leaf cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load cursor mitm leaf cert: %w", err)
	}
	ca.cache[host] = &cert
	return &cert, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate cert serial: %w", err)
	}
	return serial, nil
}

func (p *Proxy) cursorCA() (*cursorCertAuthority, error) {
	p.certMu.Lock()
	defer p.certMu.Unlock()
	if p.ca != nil {
		return p.ca, nil
	}
	ca, err := newCursorCertAuthority()
	if err != nil {
		return nil, err
	}
	p.ca = ca
	return ca, nil
}
