package robotworker

// The image server's build script, run with bash against fake commands and the real zstd.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const imagerSecret = "a cluster key imager printed"

func TestBuildAndServe_BuildsTheMetalImageDiscardingImagerOutputAndServesItToTheWorker(t *testing.T) {
	tools := testutil.NewTools(t, map[string]string{
		"ip": `[ "$1" != -4 ] || echo "10.0.0.1 dev enp7s0 src 10.0.0.5 uid 0"`,
		"docker": `[ "$1" = run ] || exit 0
echo "` + imagerSecret + `" && echo "` + imagerSecret + `" >&2
for a in "$@"; do
  case $a in *:/out) printf 'metal image' | zstd -q -o "${a%:/out}/metal-amd64.raw.zst" ;; esac
done`,
		"python3": `cp "$7" "$HOME/served"`,
	})
	dir := t.TempDir()
	config, out, netClass := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "out"), filepath.Join(dir, "net")
	writeFile(t, config, "a worker configuration")
	require.NoError(t, os.MkdirAll(filepath.Join(netClass, "enp7s0"), 0o755))
	env := tools.Env("SERVE_OUT_DIR="+out, "SERVE_RESOLV_CONF="+filepath.Join(dir, "resolv.conf"), "SERVE_NET_CLASS="+netClass)

	output, err := testutil.RunBash(t, buildAndServeScript, env, image.PinnedImager, config, "10.0.0.1",
		"python3", "-I", "serve.py", "10.0.1.2", "8443", "tls.crt", "tls.key", out+"/image.raw")

	require.NoError(t, err, output)
	assert.NotContains(t, output, imagerSecret)
	assert.NoFileExists(t, config, "the configuration is deleted once built into the image")
	calls := tools.Calls(t)
	assert.Contains(t, calls, "ip route replace default via 10.0.0.1")
	assert.Contains(t, calls, "docker run --rm --privileged -v /dev:/dev -v "+config+":/in/config.yaml:ro -v "+out+":/out "+
		image.PinnedImager+" metal --arch amd64 --embedded-config-path /in/config.yaml")
	assert.Equal(t, "python3 -I serve.py 10.0.1.2 8443 tls.crt tls.key "+out+"/image.raw", calls[len(calls)-1])
	served, err := os.ReadFile(filepath.Join(tools.Dir, "served"))
	require.NoError(t, err)
	assert.Equal(t, "metal image", string(served), "the image is served decompressed")
}
