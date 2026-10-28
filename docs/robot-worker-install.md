# How a Robot worker is installed

This page explains how the package `robotworker` turns an adopted Hetzner Robot
server into a Talos worker of the cluster. It is for the engineers who change the
install or the block's wiring of it, which sits in `worker.go`. A Robot server is a
dedicated machine rented by the month. Its rescue system is a small Linux that
Robot boots over the network when asked, from which the install writes the
disk. The [glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
defines block, kit and operator.

The install follows what one live trial measured, recorded in the
note [Talos Linux on a Hetzner Robot server behind a vSwitch gateway](https://github.com/mitstack-eu/mitstack-workspace/blob/main/product/knowledge/2026-10-28-talos-on-a-hetzner-robot-server-behind-a-vswitch-gateway.md).
Robot's own calls come from block-kit's package `capabilities/hetzner/robot`,
and the worker's configuration and image from talos-cluster-kit.

## What a run does

`Installer.Install` in `robotworker/install.go` runs once, from adoption to the
reset that boots the written disk. A run takes longer than the two minutes a
subscribe waits for each answer, so the block starts it in the background.

| Install part | Where it lives | Note section it follows |
|---|---|---|
| Refuse a server outside fsn1, nbg1 and hel1 before any write | `Install` | How the vSwitch and the Robot firewall carry the gateway |
| Adopt and name the server, then couple it to the cluster's vSwitch | `robot.Adopt`, `robot.Couple` | How the trial ran |
| Write the worker firewall: TCP, DNS and NTP replies, ICMP from the network, and the cluster's own flows, with `whitelist_hos` true | `Installer.firewall` | How the vSwitch and the Robot firewall carry the gateway |
| Admit SSH from the installer's address alone, boot the rescue system with a one-time key, and expect no host key | `robot.OpenRescueWindow`, `robot.BootRescue` | How the rescue boot depends on the firewall |
| Read the firmware, the public card's MAC and each disk's serial | `readHardware`, `rescue.sh read` | How the image reaches the disks |
| Refuse a server that did not boot by UEFI | `readHardware` | How the firmware boots the unified kernel image |
| Render the worker with its hostname and vSwitch link, found by MAC | `nodeconfig.RenderWorker` | How the node gets its name and addresses |
| Build the `metal` image on a short-lived Cloud server, the image server | `build-and-serve.sh` | What the kit lacks for a Robot node |
| Bring up the vSwitch VLAN and wait until the image server answers with the pinned key, then stop every array, clear every signature, and write the image onto the disk with the first serial | `rescue.sh write` | How the image reaches the disks |
| Delete the key, restore the worker firewall and reset with no rescue active | `install.cleanUp` | What Robot answered |

The install chooses its disk by serial because the kernel's disk names changed
between boots on the trial. Of two disks, it takes the one whose serial sorts
first, so a repeated install picks the same disk. The rescue system clears no
disk until the image server has answered over the vSwitch, so a missing curl, a
broken VLAN or an image server that never starts leaves the disks as they were.

## How the cluster's keys reach the disk

Robot returns no host key for the rescue system, as it did on the trial, so
the installer cannot tell the real rescue system from an attacker on the
public path. The SSH session therefore carries nothing secret. It runs the
fixed script `robotworker/scripts/rescue.sh`, whose arguments are a VLAN, two
addresses, a port, a key pin and a disk serial, each matched against a pattern
before it is sent.

The image embeds the worker's machine configuration, which holds the join
token and the cluster's certificate authorities. It travels another way:

1. The install generates a TLS key for this run alone, and its pin, the
   SHA-256 of the public key.
2. The Cloud API creates the image server with the configuration and the TLS
   key in its user data. That call runs over Hetzner's authenticated API, which
   an attacker on the path cannot impersonate.
3. The image server builds the `metal` image with imager, discarding imager's
   output, which prints every key.
4. The rescue system brings up the vSwitch VLAN with MTU 1400 and fetches the
   image with `curl --pinnedpubkey`, so it accepts only the server holding that
   TLS key.
5. `robotworker/scripts/serve-image.py` serves one whole copy to the worker's
   vSwitch address alone. It deletes the TLS key once loaded, and the image once
   the worker read a whole copy and closed the connection. The install then
   deletes the image server.

An attacker who answers the rescue system's SSH learns only the pin and the
addresses. An attacker who answers on the vSwitch cannot present the pinned
key. The image server knows its client only by source address, so a server on
the same vSwitch that claimed the worker's address could fetch the image. Only
servers of the same Robot account can join that vSwitch.

When Robot does name host keys, the session accepts only those, by MD5 or
SHA-256 fingerprint. No live answer has shown the form Robot names them in,
since the trial got none.

The test `TestInstall_SendsNoSecretOverTheRescueSession` in
`robotworker/install_test.go` pins that no fixture key, no TLS key, no Robot
password and no configuration appear in any command or input the rescue
session received.

## What the block provides

The package defines the interface `Cloud` and leaves its Hetzner Cloud side to
the block. `imageCloud` in `worker.go` implements it. Before it creates the
image server, it couples the vSwitch to the cluster's Cloud network as the
subnet `10.0.1.0/24`, with the network's routes exposed to the vSwitch, which
the trial needed for the route to the gateway. It then waits until the gateway
holds `10.0.0.2`, since a server that joined first would take that address.
It creates the image server in the server's location, from the image
`docker-ce`, labelled with the role `worker-image`, and deletes it after.

The block also passes `Installer.From`, the public IPv4 address the operator's
SSH leaves from. It reads that address from the operator's environment variable
`MITSTACK_EGRESS_IPV4`, and the Robot firewall admits SSH from that address
alone while the rescue system runs.

`startWorkerInstall` in `worker.go` starts `Installer.Install` in the
background of the call that creates the gateway and the builder, with a limit of
one hour. [How a subscribe runs](how-a-subscribe-runs.md#how-the-worker-is-installed)
explains its progress labels.

## What the tests prove

The tests in `robotworker/` run with `go test ./robotworker/` and need bash,
curl, python3 and zstd on the machine.

| Test | What it proves |
|---|---|
| `TestInstall_WritesTheMetalImageOverTheVSwitchInEachLocation` | In fsn1, nbg1 and hel1 the real rescue script, curl and dd write the image onto the disk with the first serial, over the VLAN with MTU 1400 |
| `TestInstall_RefusesALocationOutsideTheThreeAndChangesNothing` | A server in another location is refused, naming fsn1, nbg1 and hel1, with no Robot write |
| `TestInstall_SendsNoSecretOverTheRescueSession` | The rescue session carries no key, as above |
| `TestInstall_RefusesARescueSystemWhoseHostKeyRobotDidNotName` | A rescue system presenting another host key runs nothing |
| `TestInstall_StopsAndCleansUpWhenTheImageServerPresentsAnotherKey` | The rescue writes nothing when the pin does not match, and the run cleans up |
| `TestInstall_LeavesTheDisksAloneWhenTheImageServerNeverAnswers` | No disk is cleared when the image server never answers, and the run cleans up |
| `TestInstall_GivesUpOnARescueSystemThatNeverAnswersTheHandshake` | A rescue system that accepts the connection and stalls ends the run with its context, and the key and the firewall are cleaned up |
| `TestBuildAndServe_BuildsTheMetalImageDiscardingImagerOutputAndServesItToTheWorker` | The image server builds the `metal` image and serves it decompressed |

The Robot fake is block-kit's `robottest`, the rescue system is an SSH server
in the test, and the image server is `serve-image.py` itself on `127.0.0.1`.

## What no run has shown yet

No live run has used this package. Whether Hetzner's rescue system carries
curl is unproven, since the trial streamed the image through SSH. Whether a
Robot worker joins a Cloud control plane over this path is the trial's own
open question, as are the Cilium MTU over the vSwitch and whether Hetzner
deletes a network with a vSwitch still coupled.
