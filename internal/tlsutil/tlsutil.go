// Package tlsutil prépare la configuration HTTPS de Gná : certificat fourni,
// ou certificat auto-signé généré au premier démarrage.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFile = "tls/gna.crt"
	keyFile  = "tls/gna.key"
	validity = 3 * 365 * 24 * time.Hour
)

// Paths renvoie les chemins du certificat auto-signé dans dataDir.
func Paths(dataDir string) (cert, key string) {
	return filepath.Join(dataDir, certFile), filepath.Join(dataDir, keyFile)
}

// EnsureSelfSigned crée le certificat auto-signé s'il n'existe pas encore.
// Il couvre le nom de la machine, localhost et toutes ses adresses IP actuelles.
func EnsureSelfSigned(dataDir string) (certPath, keyPath string, created bool, err error) {
	certPath, keyPath = Paths(dataDir)
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return certPath, keyPath, false, nil
		}
	}
	return certPath, keyPath, true, Generate(dataDir)
}

// Generate (re)crée le certificat auto-signé.
func Generate(dataDir string) error {
	certPath, keyPath := Paths(dataDir)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return fmt.Errorf("création de %s : %w", filepath.Dir(certPath), err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	dns, ips := localNames(host)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Gná " + host, Organization: []string{"Gná (auto-signé)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return err
	}
	return writePEM(certPath, "CERTIFICATE", der, 0o644)
}

// Config construit une configuration TLS moderne (TLS 1.2 minimum).
func Config(certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("chargement du certificat : %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		CipherSuites: []uint16{ // utilisées en TLS 1.2 uniquement ; TLS 1.3 choisit seul
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}, nil
}

// Info décrit un certificat : empreinte SHA-256, noms couverts, validité.
type Info struct {
	Fingerprint string
	Names       []string
	NotAfter    time.Time
}

func Describe(certPath string) (*Info, error) {
	data, err := os.ReadFile(certPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("aucun certificat dans %s (il est créé au premier démarrage du service)", certPath)
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("certificat PEM illisible")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(c.Raw)
	hexs := make([]string, len(sum))
	for i, b := range sum {
		hexs[i] = fmt.Sprintf("%02X", b)
	}
	names := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	return &Info{Fingerprint: strings.Join(hexs, ":"), Names: names, NotAfter: c.NotAfter}, nil
}

// IsLoopback indique si l'adresse d'écoute n'est joignable que depuis la machine.
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false // ":8080" = toutes les interfaces
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localNames(host string) ([]string, []net.IP) {
	dns := []string{"localhost"}
	if host != "" && host != "localhost" {
		dns = append(dns, host)
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			ips = append(ips, n.IP)
		}
	}
	return dns, ips
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
