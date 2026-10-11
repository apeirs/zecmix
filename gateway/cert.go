package gateway

import (
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
	"os"
	"path/filepath"
	"time"
)

// LocalCert loads (or creates) a self-signed certificate for localhost,
// 127.0.0.1 and ::1 in dir. Some wallets only accept https:// servers; point
// them at https://localhost:PORT and trust cert.pem.
// Returns the TLS config, the certificate path and its SHA-256 fingerprint.
func LocalCert(dir string) (*tls.Config, string, string, error) {
	certPath := filepath.Join(dir, "gateway-cert.pem")
	keyPath := filepath.Join(dir, "gateway-key.pem")
	if _, err := os.Stat(certPath); err != nil {
		if err := writeCert(certPath, keyPath); err != nil {
			return nil, "", "", err
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", "", err
	}
	fp := sha256.Sum256(pair.Certificate[0])
	return &tls.Config{Certificates: []tls.Certificate{pair}, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12},
		certPath, hex.EncodeToString(fp[:]), nil
}

func writeCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "zecmix local gateway"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}
