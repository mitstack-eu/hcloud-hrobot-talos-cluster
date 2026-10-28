package robotworker

// A rescue system's SSH server and a Cloud that runs the image server's own serve program, so an install runs the
// real rescue script, curl and dd end to end against fake disks.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

const (
	testMAC = "9c:6b:00:55:d8:f5"
	// testDisk carries the serial that sorts first, on the kernel name listed second, as on the trial's server.
	testDisk, testOtherDisk = "nvme1n1", "nvme0n1"
	oldDisk                 = "the disk as the earlier install left it"
)

var testSerials = map[string]string{testOtherDisk: "S675NF0WC35475", testDisk: "S675NF0WC35363"}

// rescueHost admits a key only while Robot reads the server booted into its rescue with that key, and runs each
// command with bash against fake disks and commands.
type rescueHost struct {
	robot  *robottest.Fake
	number int64
	signer ssh.Signer
	addr   string
	dev    string
	tools  *testutil.Tools
	env    []string

	mu   sync.Mutex
	sent []string
}

func newRescueHost(t *testing.T, fake *robottest.Fake, number int64) *rescueHost {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(private)
	require.NoError(t, err)
	sys, dev := t.TempDir(), t.TempDir()
	for name, serial := range testSerials {
		writeFile(t, filepath.Join(sys, "block", name, "device", "serial"), serial+"\n")
		writeFile(t, filepath.Join(sys, "block", name, "size"), "1000215216\n")
		writeFile(t, filepath.Join(sys, "block", name, name+"p1", "partition"), "1\n")
		writeFile(t, filepath.Join(dev, name), oldDisk)
		writeFile(t, filepath.Join(dev, name+"p1"), "")
	}
	writeFile(t, filepath.Join(sys, "class", "net", "eth0", "address"), testMAC+"\n")
	require.NoError(t, os.MkdirAll(filepath.Join(sys, "firmware", "efi"), 0o755))
	tools := testutil.NewTools(t, map[string]string{
		"ip":     `[ "$*" != "-o route show default" ] || echo "default via 203.0.113.1 dev eth0 proto static"`,
		"wipefs": "", "sgdisk": "", "mdadm": "", "vgs": "", "vgchange": "",
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	h := &rescueHost{robot: fake, number: number, signer: signer, addr: ln.Addr().String(), dev: dev, tools: tools,
		env: tools.Env("RESCUE_SYS="+sys, "RESCUE_DEV="+dev, "RESCUE_FETCH_INTERVAL=0.1", "RESCUE_FETCH_ATTEMPTS=100")}
	config := &ssh.ServerConfig{PublicKeyCallback: h.admit}
	config.AddHostKey(signer)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.handle(conn, config)
		}
	}()
	return h
}

func (h *rescueHost) admit(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	s := h.robot.State(h.number)
	if s.Booted == "rescue" && slices.Contains(s.RescueKeys, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))) {
		return nil, nil
	}
	return nil, errors.New("this key is not admitted to the rescue system")
}

func (h *rescueHost) handle(conn net.Conn, config *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			continue
		}
		go h.exec(ch, requests)
	}
}

func (h *rescueHost) exec(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	for req := range requests {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var command struct{ Command string }
		_ = ssh.Unmarshal(req.Payload, &command)
		_ = req.Reply(true, nil)
		var stdin bytes.Buffer
		cmd := exec.Command("bash", "-c", command.Command)
		cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = h.env, io.TeeReader(ch, &stdin), ch, ch.Stderr()
		status := 0
		if err := cmd.Run(); err != nil {
			status = 1
		}
		h.mu.Lock()
		h.sent = append(h.sent, command.Command, stdin.String())
		h.mu.Unlock()
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
		return
	}
}

func (h *rescueHost) disk(t *testing.T, name string) string {
	data, err := os.ReadFile(filepath.Join(h.dev, name))
	require.NoError(t, err)
	return string(data)
}

// fakeCloud runs the serve program from the image server's user data on 127.0.0.1, serving an image that embeds the
// worker configuration the user data carries, and with otherKey a TLS key other than the pinned one.
type fakeCloud struct {
	t        *testing.T
	otherKey bool
	created  []ImageServer
	deleted  []int64
	image    string
	dir      string
	serve    *exec.Cmd
	exited   chan struct{}
}

func (c *fakeCloud) CreateImageServer(_ context.Context, s ImageServer) (int64, error) {
	c.created = append(c.created, s)
	files := userDataFiles(c.t, s.UserData)
	c.dir = c.t.TempDir()
	cert, key := files[serveCertPath], files[serveKeyPath]
	if c.otherKey {
		other, err := newServeKey()
		require.NoError(c.t, err)
		cert, key = other.certPEM, other.keyPEM
	}
	c.image = "metal image embedding\n" + files[serveConfigPath]
	for name, content := range map[string]string{"tls.crt": cert, "tls.key": key, "image.raw": c.image, "serve-image.py": files[serveProgPath]} {
		writeFile(c.t, filepath.Join(c.dir, name), content)
	}
	c.serve = exec.Command("python3", "-I", filepath.Join(c.dir, "serve-image.py"), "127.0.0.1", strconv.Itoa(servePort),
		filepath.Join(c.dir, "tls.crt"), filepath.Join(c.dir, "tls.key"), filepath.Join(c.dir, "image.raw"))
	require.NoError(c.t, c.serve.Start())
	c.exited = make(chan struct{})
	go func() { _ = c.serve.Wait(); close(c.exited) }()
	return int64(len(c.created)), nil
}

func (c *fakeCloud) ImageServerAddress(context.Context, int64) (netip.Addr, error) {
	return netip.MustParseAddr("127.0.0.1"), nil
}

// DeleteImageServer gives the serve program a moment to exit on its own once it served, before it stops it.
func (c *fakeCloud) DeleteImageServer(_ context.Context, id int64) error {
	c.deleted = append(c.deleted, id)
	select {
	case <-c.exited:
	case <-time.After(2 * time.Second):
		_ = c.serve.Process.Kill()
		<-c.exited
	}
	return nil
}

// userDataFiles decodes every file of a cloud-config, gunzipping those cloud-init would.
func userDataFiles(t *testing.T, userData string) map[string]string {
	body, ok := strings.CutPrefix(userData, "#cloud-config\n")
	require.True(t, ok, "the user data is no cloud-config")
	var config cloudConfig
	require.NoError(t, yaml.Unmarshal([]byte(body), &config))
	files := map[string]string{}
	for _, f := range config.WriteFiles {
		content := f.Content
		if f.Encoding == "gz+b64" {
			raw, err := base64.StdEncoding.DecodeString(content)
			require.NoError(t, err)
			r, err := gzip.NewReader(bytes.NewReader(raw))
			require.NoError(t, err)
			unzipped, err := io.ReadAll(r)
			require.NoError(t, err)
			content = string(unzipped)
		}
		files[f.Path] = content
	}
	return files
}

func writeFile(t *testing.T, path, content string) {
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
