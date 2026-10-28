package robotworker

// The install of a Robot worker against the Robot fake, a rescue system's SSH server and a Cloud serving the image.

import (
	"context"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

const testServer = 3041839

var apidRule = robot.Rule{Name: "gateway to apid", IPVersion: "ipv4", SrcIP: "10.0.0.2/32", DstIP: "10.0.1.2/32", Protocol: "tcp", DstPort: "50000", Action: "accept"}

type harness struct {
	robot     *robottest.Fake
	rescue    *rescueHost
	cloud     *fakeCloud
	installer Installer
	worker    Worker
}

func newHarness(t *testing.T, dc string) *harness {
	fake := robottest.New(t)
	fake.AddServer(robottest.Server{Number: testServer, IP: "203.0.113.10", DC: dc})
	rescue := newRescueHost(t, fake, testServer)
	cloud := &fakeCloud{t: t}
	servePort = freePort(t)
	return &harness{
		robot: fake, rescue: rescue, cloud: cloud,
		installer: Installer{
			Robot: fake.Client(), Cloud: cloud, Cluster: testutil.NewCluster(t), From: netip.MustParseAddr("198.51.100.7"),
			RescueAddress: func(string) string { return rescue.addr }, DialInterval: 10 * time.Millisecond,
		},
		worker: Worker{
			Server: testServer, Name: "mitstack-shop-wk1", VSwitch: "mitstack-shop", Hostname: "shop-wk1",
			Address: netip.MustParsePrefix("10.0.1.2/24"), Rules: []robot.Rule{apidRule},
		},
	}
}

func (h *harness) install(*testing.T) error { return h.installWithin(time.Minute) }

func (h *harness) installWithin(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return h.installer.Install(ctx, h.worker)
}

func ruleNames(rules []robot.Rule) []string {
	var names []string
	for _, r := range rules {
		names = append(names, r.Name)
	}
	return names
}

// assertCleanedUp checks the install left no rescue key, no image server and no rescue SSH rule, and booted the disk.
func assertCleanedUp(t *testing.T, h *harness) {
	assert.Empty(t, h.robot.Keys(), "the one-time rescue key is deleted")
	assert.Equal(t, []int64{1}, h.cloud.deleted, "the image server is deleted")
	assert.Equal(t, ruleNames(Firewall(h.installer.Cluster.Network, h.worker).Input), ruleNames(h.robot.Firewall(testServer).Input),
		"the worker firewall returns without the rescue SSH rule")
	assert.Equal(t, "disk", h.robot.State(testServer).Booted)
}

func TestInstall_WritesTheMetalImageOverTheVSwitchInEachLocation(t *testing.T) {
	for _, tc := range []struct{ location, dc, hostKey string }{
		{"fsn1", "FSN1-DC14", ""},
		{"nbg1", "NBG1-DC3", "md5"},
		{"hel1", "HEL1-DC1", "sha256"},
	} {
		t.Run(tc.location, func(t *testing.T) {
			h := newHarness(t, tc.dc)
			switch tc.hostKey {
			case "md5":
				h.robot.HostKeys = []string{ssh.FingerprintLegacyMD5(h.rescue.signer.PublicKey())}
			case "sha256":
				h.robot.HostKeys = []string{ssh.FingerprintSHA256(h.rescue.signer.PublicKey())}
			}

			require.NoError(t, h.install(t))

			assert.Equal(t, "mitstack-shop-wk1", h.robot.State(testServer).Name)
			vswitches := h.robot.VSwitches()
			require.Len(t, vswitches, 1)
			assert.Equal(t, robot.FirstVLAN, vswitches[0].VLAN)
			assert.Equal(t, []robot.VSwitchServer{{Number: testServer, Status: "ready"}}, vswitches[0].Servers)

			resets := h.robot.FirewallsAtReset(testServer)
			require.Len(t, resets, 2, "one reset boots the rescue system and one the written disk")
			assert.Contains(t, resets[0].Input, robot.Rule{Name: "rescue ssh", IPVersion: "ipv4", SrcIP: "198.51.100.7/32", DstIP: "203.0.113.10/32", Protocol: "tcp", DstPort: "22", Action: "accept"})
			assert.True(t, resets[0].WhitelistHOS)
			assert.Contains(t, h.robot.Firewall(testServer).Input, apidRule)
			assertCleanedUp(t, h)

			require.Len(t, h.cloud.created, 1)
			assert.Equal(t, ImageServer{Name: "mitstack-shop-wk1-image", Location: tc.location, Image: "docker-ce", UserData: h.cloud.created[0].UserData}, h.cloud.created[0])
			config := userDataFiles(t, h.cloud.created[0].UserData)[serveConfigPath]
			assert.Contains(t, config, `mac(link.permanent_addr) == "`+testMAC+`"`)
			assert.Contains(t, config, "shop-wk1")

			assert.Equal(t, h.cloud.image, h.rescue.disk(t, testDisk), "the image lands on the disk with the first serial")
			assert.Equal(t, oldDisk, h.rescue.disk(t, testOtherDisk))
			assert.NoFileExists(t, filepath.Join(h.cloud.dir, "image.raw"), "the image server deletes the image once served")
			assert.NoFileExists(t, filepath.Join(h.cloud.dir, "tls.key"))
			dev := h.rescue.dev
			assert.Subset(t, h.rescue.tools.Calls(t), []string{
				"wipefs -a " + dev + "/" + testOtherDisk + "p1",
				"sgdisk --zap-all " + dev + "/" + testDisk,
				"ip link add link eth0 name vsw0 type vlan id 4000",
				"ip link set vsw0 mtu 1400 up",
				"ip addr add 10.0.1.2/24 dev vsw0",
				"ip route add 127.0.0.1/32 via 10.0.1.1 dev vsw0",
				"ip link del vsw0",
			})
		})
	}
}

func TestInstall_RefusesALocationOutsideTheThreeAndChangesNothing(t *testing.T) {
	h := newHarness(t, "ASH-DC1")

	err := h.install(t)

	var refused *handler.Refused
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "location-unsupported", refused.Class)
	assert.Contains(t, err.Error(), "fsn1, nbg1 or hel1")
	assert.Empty(t, h.robot.Writes())
	assert.Empty(t, h.cloud.created)
}

func TestInstall_SendsNoSecretOverTheRescueSession(t *testing.T) {
	h := newHarness(t, "FSN1-DC14")
	require.NoError(t, h.install(t))
	files := userDataFiles(t, h.cloud.created[0].UserData)
	secrets := append(testutil.SecretValues(t), robottest.Password, files[serveKeyPath], files[serveConfigPath])
	require.True(t, slices.ContainsFunc(testutil.SecretValues(t), func(s string) bool { return strings.Contains(files[serveConfigPath], s) }),
		"the image server's user data carries the cluster's keys, so the check below can find one")

	require.NotEmpty(t, h.rescue.sent)
	for _, sent := range h.rescue.sent {
		for _, secret := range secrets {
			assert.NotContains(t, sent, secret)
		}
	}
}

func TestInstall_RefusesARescueSystemWhoseHostKeyRobotDidNotName(t *testing.T) {
	h := newHarness(t, "HEL1-DC1")
	h.robot.HostKeys = []string{"SHA256:bQg19BgeJ2qso3fT1u1q9RrW25lITHoeLNIli/UWguY"}

	err := h.install(t)

	require.ErrorContains(t, err, "none of the host keys Robot named")
	assert.Empty(t, h.rescue.sent, "nothing runs in a rescue system presenting another host key")
	assert.Empty(t, h.cloud.created)
	assert.Empty(t, h.robot.Keys())
	assert.Equal(t, ruleNames(Firewall(h.installer.Cluster.Network, h.worker).Input), ruleNames(h.robot.Firewall(testServer).Input))
}

func TestInstall_StopsAndCleansUpWhenTheImageServerPresentsAnotherKey(t *testing.T) {
	h := newHarness(t, "NBG1-DC3")
	h.cloud.otherKey = true

	err := h.install(t)

	require.ErrorContains(t, err, "other than the pinned")
	assert.Equal(t, oldDisk, h.rescue.disk(t, testDisk))
	assertCleanedUp(t, h)
}

func TestInstall_LeavesTheDisksAloneWhenTheImageServerNeverAnswers(t *testing.T) {
	h := newHarness(t, "FSN1-DC14")
	h.cloud.silent = true
	h.rescue.env = append(h.rescue.env, "RESCUE_FETCH_ATTEMPTS=3")

	err := h.install(t)

	require.ErrorContains(t, err, "gave no answer")
	assert.Equal(t, oldDisk, h.rescue.disk(t, testDisk))
	for _, call := range h.rescue.tools.Calls(t) {
		assert.NotRegexp(t, "^(wipefs|sgdisk|mdadm) ", call, "no disk is cleared before the image server answers")
	}
	assertCleanedUp(t, h)
}

func TestInstall_GivesUpOnARescueSystemThatNeverAnswersTheHandshake(t *testing.T) {
	h := newHarness(t, "HEL1-DC1")
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { silent.Close() })
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	h.installer.RescueAddress = func(string) string { return silent.Addr().String() }
	done := make(chan error, 1)

	go func() { done <- h.installWithin(2 * time.Second) }()

	select {
	case err = <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("the install still waits on the handshake after its context ended")
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, h.robot.Keys())
	assert.Equal(t, ruleNames(Firewall(h.installer.Cluster.Network, h.worker).Input), ruleNames(h.robot.Firewall(testServer).Input))
}
