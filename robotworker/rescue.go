package robotworker

// Robot names no host key for the rescue system, so the session runs only the fixed rescue script with arguments
// matched against patterns, and carries no secret. The image, which holds the cluster's keys, reaches the disk over
// the vSwitch from a server whose TLS key the rescue checks against a pin.

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/handler"
	"golang.org/x/crypto/ssh"
)

//go:embed scripts/rescue.sh
var rescueScript []byte

const (
	rescueUser        = "root"
	rescueDialTimeout = 10 * time.Second
	maxRescueOutput   = 64 << 10
)

var (
	serialRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	pinRe    = regexp.MustCompile(`^sha256//[A-Za-z0-9+/]{43}=$`)
)

// hostKeyCheck accepts only a host key whose MD5 or SHA256 fingerprint Robot named, and otherwise the presented key,
// since the session then carries no secret.
func hostKeyCheck(s robot.RescueSession) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		md5, sha := ssh.FingerprintLegacyMD5(key), ssh.FingerprintSHA256(key)
		if len(s.HostKeys) == 0 {
			log.Printf("robotworker: server %d: Robot named no host key for the rescue system, so the session accepts %s and sends it no secret", s.Server, sha)
			return nil
		}
		if slices.Contains(s.HostKeys, md5) || slices.Contains(s.HostKeys, sha) {
			return nil
		}
		return &hostKeyRefused{fmt.Errorf("the rescue system of server %d presented host key %s, none of the host keys Robot named (%s)", s.Server, sha, strings.Join(s.HostKeys, ", "))}
	}
}

// hostKeyRefused marks a handshake the host key check refused, which no retry can fix.
type hostKeyRefused struct{ error }

func (e *hostKeyRefused) Unwrap() error { return e.error }

// dialRescue retries while the rescue system still boots, and stops at once on a host key it refuses.
func dialRescue(ctx context.Context, addr string, key robot.RescueKey, s robot.RescueSession, interval time.Duration) (*ssh.Client, error) {
	signer, err := ssh.NewSignerFromKey(key.PrivateKey())
	if err != nil {
		return nil, fmt.Errorf("wrapping the rescue key: %w", err)
	}
	config := &ssh.ClientConfig{User: rescueUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKeyCheck(s), Timeout: rescueDialTimeout}
	for {
		client, err := dial(ctx, addr, config)
		var refused *hostKeyRefused
		switch {
		case err == nil:
			return client, nil
		case errors.As(err, &refused):
			return nil, refused.error
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the rescue system of server %d never admitted the session's key: %w (last attempt: %v)", s.Server, ctx.Err(), err)
		case <-time.After(interval):
		}
	}
}

func dial(ctx context.Context, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := (&net.Dialer{Timeout: rescueDialTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// The handshake takes no context, so a rescue system that accepts the connection and stalls is cut off here.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(rescueDialTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if !stop() || err != nil {
		conn.Close()
		return nil, errors.Join(err, ctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// runRescue runs the rescue script with args, and returns its output, ended at maxRescueOutput.
func runRescue(ctx context.Context, client *ssh.Client, args ...string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("opening a rescue session: %w", err)
	}
	defer session.Close()
	var out rescueOutput
	session.Stdin, session.Stdout, session.Stderr = bytes.NewReader(rescueScript), &out, &out
	done := make(chan error, 1)
	go func() { done <- session.Run(strings.Join(append([]string{"bash", "-s", "--"}, args...), " ")) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("the rescue script %s failed: %w; its output: %s", args[0], err, out.String())
	}
	return out.String(), nil
}

// rescueOutput keeps the first maxRescueOutput bytes the script wrote, from stdout and stderr at once.
type rescueOutput struct {
	mu  sync.Mutex
	buf []byte
}

func (o *rescueOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf = append(o.buf, p[:min(len(p), max(maxRescueOutput-len(o.buf), 0))]...)
	return len(p), nil
}

func (o *rescueOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.buf)
}

type hardware struct {
	mac  net.HardwareAddr
	disk string
}

// readHardware picks the disk whose serial sorts first, since the kernel's disk names change between boots, and
// refuses a server that did not boot by UEFI, the only boot the trial proved.
func readHardware(ctx context.Context, client *ssh.Client) (hardware, error) {
	out, err := runRescue(ctx, client, "read")
	if err != nil {
		return hardware{}, err
	}
	var hw hardware
	efi := false
	var serials []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 2 && fields[0] == "efi":
			efi = fields[1] == "yes"
		case len(fields) == 2 && fields[0] == "mac":
			if hw.mac, err = net.ParseMAC(fields[1]); err != nil {
				return hw, fmt.Errorf("the rescue system named the MAC %q: %w", fields[1], err)
			}
		case len(fields) == 4 && fields[0] == "disk" && serialRe.MatchString(fields[2]):
			serials = append(serials, fields[2])
		}
	}
	slices.Sort(serials)
	switch {
	case !efi:
		return hw, handler.Refuse("firmware-not-uefi", "the server's firmware did not boot the rescue system by UEFI, and Talos's unified kernel image is written only for a UEFI boot")
	case hw.mac == nil:
		return hw, errors.New("the rescue system named no MAC for the card its default route leaves by")
	case len(serials) == 0:
		return hw, handler.Refuse("no-disk", "the rescue system found no NVMe or SATA disk with a serial number, and the install chooses its disk only by serial")
	}
	hw.disk = serials[0]
	return hw, nil
}

type imageFetch struct {
	server netip.Addr
	pin    string
}

func writeImage(ctx context.Context, client *ssh.Client, vlan int, address netip.Prefix, f imageFetch, serial string) error {
	gateway := address.Masked().Addr().Next()
	if !serialRe.MatchString(serial) || !pinRe.MatchString(f.pin) || !f.server.Is4() || !address.Addr().Is4() {
		return fmt.Errorf("refusing to send the rescue system the serial %q, the pin %q or the addresses %s and %s, which match no pattern", serial, f.pin, f.server, address)
	}
	_, err := runRescue(ctx, client, "write", strconv.Itoa(vlan), address.String(), gateway.String(), f.server.String(), strconv.Itoa(servePort), f.pin, serial)
	return err
}
