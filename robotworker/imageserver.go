package robotworker

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"go.yaml.in/yaml/v4"
)

var (
	//go:embed scripts/build-and-serve.sh
	buildAndServeScript string
	//go:embed scripts/serve-image.py
	serveImageProgram string
)

// servePort is a variable so a test can serve the image on a port of its own.
var servePort = 8443

const (
	serveDir        = "/root/mitstack-image"
	serveConfigPath = serveDir + "/config.yaml"
	serveCertPath   = serveDir + "/tls.crt"
	serveKeyPath    = serveDir + "/tls.key"
	serveScriptPath = "/usr/local/bin/mitstack-build-and-serve"
	serveProgPath   = "/usr/local/bin/mitstack-serve-image"
	// serveCertLifetime outlasts an install; the rescue pins the key and ignores the validity.
	serveCertLifetime = 24 * time.Hour
)

// serveKey is the image server's TLS key, its self-signed certificate and the pin curl takes for its public key.
type serveKey struct {
	certPEM, keyPEM, pin string
}

func newServeKey() (serveKey, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return serveKey{}, fmt.Errorf("generating the image server's TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return serveKey{}, fmt.Errorf("drawing the image server's certificate serial: %w", err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "mitstack Robot worker image"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(serveCertLifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &private.PublicKey, private)
	if err != nil {
		return serveKey{}, fmt.Errorf("signing the image server's certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return serveKey{}, fmt.Errorf("encoding the image server's TLS key: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		return serveKey{}, fmt.Errorf("encoding the image server's public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return serveKey{
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		pin:     "sha256//" + base64.StdEncoding.EncodeToString(sum[:]),
	}, nil
}

type cloudFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
	Encoding    string `yaml:"encoding,omitempty"`
}

type cloudConfig struct {
	WriteFiles []cloudFile `yaml:"write_files"`
	RunCmd     [][]string  `yaml:"runcmd"`
}

// imageServerUserData reaches the image server over the Cloud API, the one path into the install that an on-path
// attacker cannot impersonate, and carries the worker configuration gzipped, since as text it is over Hetzner's limit.
func imageServerUserData(config []byte, key serveKey, network netip.Prefix, worker netip.Addr) (string, error) {
	var zipped bytes.Buffer
	w := gzip.NewWriter(&zipped)
	if _, err := w.Write(config); err != nil {
		return "", fmt.Errorf("compressing the worker configuration: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("compressing the worker configuration: %w", err)
	}
	router := network.Masked().Addr().Next()
	out, err := yaml.Marshal(cloudConfig{
		WriteFiles: []cloudFile{
			{Path: serveConfigPath, Permissions: "0600", Content: base64.StdEncoding.EncodeToString(zipped.Bytes()), Encoding: "gz+b64"},
			{Path: serveCertPath, Permissions: "0600", Content: key.certPEM},
			{Path: serveKeyPath, Permissions: "0600", Content: key.keyPEM},
			{Path: serveScriptPath, Permissions: "0755", Content: buildAndServeScript},
			{Path: serveProgPath, Permissions: "0755", Content: serveImageProgram},
		},
		RunCmd: [][]string{{serveScriptPath, image.PinnedImager, serveConfigPath, router.String(), worker.String(), strconv.Itoa(servePort), serveCertPath, serveKeyPath, serveProgPath}},
	})
	if err != nil {
		return "", fmt.Errorf("encoding the image server's cloud-config: %w", err)
	}
	userData := "#cloud-config\n" + string(out)
	if len(userData) > gateway.UserDataLimit {
		return "", fmt.Errorf("the image server's user data is %d bytes, over Hetzner's %d", len(userData), gateway.UserDataLimit)
	}
	return userData, nil
}
