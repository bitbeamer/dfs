package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
)

const (
	certificateFile = "pairing-cert.pem"
	privateKeyFile  = "pairing-key.pem"
)

func loadOrCreateCertificate(repository string) (tls.Certificate, string, error) {
	directory := filepath.Join(repository, filepath.FromSlash(config.Directory))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create DFS state directory: %w", err)
	}
	certPath := filepath.Join(directory, certificateFile)
	keyPath := filepath.Join(directory, privateKeyFile)
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		fingerprint, fingerprintErr := certificateFingerprint(certificate)
		return certificate, fingerprint, fingerprintErr
	}
	if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, "", fmt.Errorf("load pairing certificate: %w", err)
	}
	lockPath := filepath.Join(directory, "pairing-cert.lock")
	var lock *os.File
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return tls.Certificate{}, "", fmt.Errorf("lock pairing certificate: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		if certificate, loadErr := tls.LoadX509KeyPair(certPath, keyPath); loadErr == nil {
			fingerprint, fingerprintErr := certificateFingerprint(certificate)
			return certificate, fingerprint, fingerprintErr
		}
		if time.Now().After(deadline) {
			return tls.Certificate{}, "", errors.New("timed out waiting for pairing certificate creation")
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = lock.Close()
	defer os.Remove(lockPath)
	// Another process may have completed the keypair just before this process
	// acquired the lock.
	if certificate, loadErr := tls.LoadX509KeyPair(certPath, keyPath); loadErr == nil {
		fingerprint, fingerprintErr := certificateFingerprint(certificate)
		return certificate, fingerprint, fingerprintErr
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate pairing key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate certificate serial: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "DFS local pairing"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create pairing certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("encode pairing key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeExclusivePair(certPath, keyPath, certPEM, keyPEM); err != nil {
		return tls.Certificate{}, "", err
	}
	certificate, err = tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("load generated pairing certificate: %w", err)
	}
	fingerprint, err := certificateFingerprint(certificate)
	return certificate, fingerprint, err
}

func writeExclusivePair(certPath, keyPath string, certPEM, keyPEM []byte) error {
	// The certificate is not a trust root by itself; invitations pin it. Atomic
	// writes avoid exposing a partial keypair if the process is interrupted.
	temporaryKey := keyPath + ".new"
	temporaryCert := certPath + ".new"
	if err := os.WriteFile(temporaryKey, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write pairing key: %w", err)
	}
	defer os.Remove(temporaryKey)
	if err := os.WriteFile(temporaryCert, certPEM, 0o600); err != nil {
		return fmt.Errorf("write pairing certificate: %w", err)
	}
	defer os.Remove(temporaryCert)
	if err := os.Rename(temporaryKey, keyPath); err != nil {
		return fmt.Errorf("install pairing key: %w", err)
	}
	if err := os.Rename(temporaryCert, certPath); err != nil {
		return fmt.Errorf("install pairing certificate: %w", err)
	}
	return nil
}

func certificateFingerprint(certificate tls.Certificate) (string, error) {
	if len(certificate.Certificate) == 0 {
		return "", errors.New("pairing certificate contains no leaf certificate")
	}
	digest := sha256.Sum256(certificate.Certificate[0])
	return hex.EncodeToString(digest[:]), nil
}
