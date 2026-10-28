# Build progress and failures

This page says what hcloud-hrobot-talos-cluster reports while the cluster builds,
and every reason a subscribe can be refused, with what you do about each. It is
for anyone watching a subscribe that runs long or reading a refusal.
[How a subscribe runs](how-a-subscribe-runs.md) explains the stages around the
build, and
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
defines words such as validate and subscribe.

## What a build reports while it runs

The Cloud control plane's builder makes the cluster's disk image. No session
can watch it, so Hetzner's CPU and disk metrics of the builder are the view of
a running build. The `controlplane` package of
[talos-cluster-kit](https://github.com/mitstack-eu/talos-cluster-kit/blob/main/README.md#how-a-block-drives-the-cloud-control-plane)
reads them and words the summary, and the cluster resource's validate detail
carries it while the builder runs. It gives the CPU over the last minute, the
bytes written to disk over the last minute, and the bytes written to disk since
the builder's creation.

The worker has its own progress, which is the Cloud network's label
`mitstack.dev/worker-install`. While the install runs, the cluster resource's
validate detail reads "the Robot worker's install has run since" and a time. The
install itself logs through the operator's `/logs` route, with the request's secrets removed.

## When a build fails

A builder still running 30 minutes after its creation has failed, since its
script leaves a failed build running. Validate then asks for a re-drive, and
that subscribe deletes the builder and refuses with `image-build-failed`. The
refusal carries the metrics summary, when the disk last moved and when the CPU
was last above 10%. A metrics read that fails never fails validate or the
refusal.

A worker install that ends in an error marks the network `failed`, and the
operator's log says why. The next subscribe refuses with `worker-install-failed`.

## Refusals and what to do about them

Every refusal comes before the call changes anything unless the table says
otherwise. The class is the name that follows `refused:` in the answer.

| Class | Meaning | What you do |
|---|---|---|
| `missing-param` | `appName`, or `robotServerNumber`, is empty | Fill in the param and subscribe again |
| `invalid-param` | `serverType` is not a listed type, `hostsPlatform` is true, or `robotServerNumber` is not a positive whole number | Fix the param |
| `missing-hetzner-access` | The Hetzner connection holds no `api_token` | Connect a Hetzner Cloud project token and retry |
| `missing-robot-access` | The Hetzner connection holds no `robot_user` or no `robot_password` | Add the Robot webservice login to the connection and retry |
| `missing-github-access` | The GitHub connection is unusable | Connect GitHub and retry |
| `egress-address-unset` | The operator's `MITSTACK_EGRESS_IPV4` holds no IPv4 address, and the dedicated server's firewall admits the rescue session from that address alone | Set the variable on the operator's host to its public IPv4 address |
| `server-not-found` | Robot holds no dedicated server with that number in the account | Check the number and the Robot account |
| `server-cancelled` | The server is cancelled, so no worker is installed over it | Name a server that is not cancelled |
| `location-unsupported` | The server is not in `fsn1`, `nbg1` or `hel1`, where its vSwitch reaches the Cloud network | Name a server in one of those locations |
| `server-taken` | The server carries the name of another mitstack subscription | Name another server, or unsubscribe the other subscription |
| `worker-install-lost` | The operator restarted during the worker's install, and the cluster's keys went with it | Unsubscribe and subscribe again |
| `worker-install-failed` | The worker's install ended in an error, and the operator's log says why | Unsubscribe and subscribe again |
| `vswitch-lost` | No vSwitch named for the subscription is left, and the worker boots on its VLAN alone | Unsubscribe and subscribe again |
| `ambiguous-vswitch` | More than one vSwitch carries the subscription's name | Cancel the extra ones in Robot and retry |
| `repository-not-marked` | The cluster repository exists without the subscription's mark, two GitHub topics | Add both topics if it is the cluster's repository, or move it away, and retry |
| `image-build-failed` | The builder did not power itself off within 30 minutes | Unsubscribe and subscribe again |
| `ambiguous-cluster` | Two Cloud servers of one role in one location, or two load balancers of one role, carry the subscription's labels | Delete the extra ones from the Hetzner project and retry |
| `server-unavailable` | Hetzner cannot place a server, or `fsn1` can order neither gateway type | Subscribe again later |
| `gateway-lost` | The nodes run but no gateway carries the subscription's labels | Unsubscribe and subscribe again |
| `robot-left-in-place` | At unsubscribe, the Robot login was unusable, so the Cloud resources are deleted but the server and its vSwitch stay | Fix the login in the connection and unsubscribe again |
| `repository-left-in-place` | At unsubscribe, the GitHub connection was unusable, so the repository stays | Fix the connection and unsubscribe again |

The kit's `controlplane` package raises `image-build-failed`, `ambiguous-cluster`,
`server-unavailable` and `gateway-lost`, and the block passes each through with
its class. An install whose create fails deletes the gateway and the builder, so
the next call installs anew. The cluster's keys exist only in the call that
mints them, the builder's user data and the snapshot, so a cluster that loses
its gateway while its nodes run is refused rather than rebuilt.

Once the worker is written, the server losing its name, its place on the vSwitch
or its firewall is not a refusal. The next subscribe puts the lost one back.
