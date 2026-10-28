package main

import (
	"context"
	"strconv"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

// EnsureResources runs Ensure and then reads back, by label and Robot name, what it converged.
func (r *blockResource) EnsureResources(ctx context.Context, d contract.Desired) ([]contract.CreatedResource, error) {
	if err := r.Ensure(ctx, d); err != nil {
		return nil, err
	}
	p := newPlane(newCloudAPI(d.Secrets["hetzner"]["api_token"]), d.SubscriptionID, d.Params["appName"])
	st, err := p.Read(ctx)
	if err != nil {
		return nil, err
	}
	out := createdResources(st, d.SubscriptionID, repoFor(d.Secrets["github"]["owner"], d.Params["appName"]))
	t, _, ok := robotReader(d)
	if !ok {
		return out, nil
	}
	robotSt, err := t.read(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, robotSt.createdResources(t)...), nil
}

// createdResources reports each declared Cloud and GitHub resource by its identity, leaving out one not yet made.
func createdResources(st controlplane.State, subID string, repo clusterRepo) []contract.CreatedResource {
	var out []contract.CreatedResource
	add := func(name, typ, key string, id int64, ok bool) {
		if ok {
			out = append(out, contract.CreatedResource{Name: name, Type: typ, Identity: map[string]string{key: strconv.FormatInt(id, 10)}})
		}
	}
	gw := controlplane.GatewayLocation
	network, networkOK := st.Network()
	add(resNetwork, resNetwork, "networkId", network.ID, networkOK)
	firewall, firewallOK := st.Firewall()
	add(resFirewall, resFirewall, "firewallId", firewall.ID, firewallOK)
	api, apiOK := st.LoadBalancerIn(controlplane.RoleAPI, gw)
	add(located(resPrivateLB, gw), typeLoadBalancer, "loadBalancerId", api.ID, apiOK)
	ingress, ingressOK := st.LoadBalancerIn(controlplane.RoleIngress, gw)
	add(located(resPublicLB, gw), typeLoadBalancer, "loadBalancerId", ingress.ID, ingressOK)
	repoIdentity := map[string]string{"repo": repo.fullName()}
	out = append(out,
		contract.CreatedResource{Name: resRepository, Type: typeRepository, Identity: repoIdentity},
		contract.CreatedResource{Name: resSeedCommit, Type: resSeedCommit, Identity: repoIdentity})
	gateway, gatewayOK := st.Server(controlplane.RoleGateway)
	add(resGateway, typeServer, "serverId", gateway.ID, gatewayOK)
	for _, loc := range controlplane.Locations {
		node, nodeOK := st.NodeIn(loc)
		add(located(resNode, loc), typeServer, "serverId", node.ID, nodeOK)
	}
	return append(out, contract.CreatedResource{Name: resCluster, Type: resCluster, Identity: map[string]string{"subscriptionId": subID}})
}

// createdResources reports the worker's Robot resources once the server carries the subscription's name.
func (st robotState) createdResources(t robotTarget) []contract.CreatedResource {
	if st.server.Name != t.name {
		return nil
	}
	server := map[string]string{"serverNumber": strconv.FormatInt(t.server, 10)}
	out := []contract.CreatedResource{
		{Name: resDedicatedServer, Type: typeDedicatedServer, Identity: server},
		{Name: resRobotFirewall, Type: typeRobotFirewall, Identity: server},
	}
	if st.vswitches == 1 {
		out = append(out, contract.CreatedResource{Name: resVSwitch, Type: typeVSwitch, Identity: map[string]string{"vswitchId": strconv.FormatInt(st.vswitch.ID, 10)}})
	}
	return out
}
