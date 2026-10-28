package robotworker

import (
	_ "embed"
	"fmt"
	"net/netip"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/serve"
)

//go:embed scripts/build-and-serve.sh
var buildAndServeScript string

// servePort is a variable so a test can serve the image on a port of its own.
var servePort = serve.DefaultPort

const (
	serveConfigPath = serve.Dir + "/config.yaml"
	serveScriptPath = "/usr/local/bin/mitstack-build-and-serve"
	// servedImage is the image the build leaves, which the rescue fetches at /image.raw.
	servedImage = "/var/lib/mitstack-image/image.raw"
)

func newServeKey() (serve.Key, error) {
	return serve.NewKey("mitstack Robot worker image", time.Now())
}

// imageServerUserData reaches the image server over the Cloud API, the one path into the install that an on-path
// attacker cannot impersonate, and carries the worker configuration gzipped, since as text it is over Hetzner's limit.
func imageServerUserData(config []byte, key serve.Key, network netip.Prefix, worker netip.Addr) (string, error) {
	zipped, err := serve.Gzipped(config)
	if err != nil {
		return "", fmt.Errorf("compressing the worker configuration: %w", err)
	}
	router := network.Masked().Addr().Next()
	files := append(key.Files(),
		serve.File{Path: serveConfigPath, Permissions: "0600", Content: zipped, Encoding: serve.GzipBase64},
		serve.File{Path: serveScriptPath, Permissions: "0755", Content: buildAndServeScript},
	)
	build := append([]string{serveScriptPath, image.PinnedImager, serveConfigPath, router.String()}, serve.Command(worker, servePort, servedImage)...)
	userData, err := serve.UserData(files, build)
	if err != nil {
		return "", fmt.Errorf("the image server: %w", err)
	}
	return userData, nil
}
