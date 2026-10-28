package main

import (
	"context"
	"fmt"
	"log"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/safe"
)

// Delete stops the worker's install, deletes every labelled Cloud resource, the snapshot that holds every key of the
// cluster among them, cancels the vSwitch and releases the dedicated server without cancelling it, and deletes the
// repository. Without a usable Robot login or GitHub connection it still deletes the Cloud resources, and then refuses,
// naming what it left in place.
func (r *blockResource) Delete(ctx context.Context, d contract.Desired) (map[string]string, error) {
	token, err := hetznerToken(d)
	if err != nil {
		return nil, err
	}
	subID, err := subscriptionID(d)
	if err != nil {
		return nil, err
	}
	if err := stopJob(ctx, subID); err != nil {
		return nil, err
	}
	gh, ghErr := dialClusterRepository(d)
	if err := newPlane(newCloudAPI(token), subID, d.Params["appName"]).Delete(ctx); err != nil {
		return nil, err
	}
	t, refused, robotOK := robotReader(d)
	if robotOK {
		if err := t.release(ctx); err != nil {
			return nil, err
		}
	}
	switch {
	case !robotOK:
		return nil, handler.Refuse("robot-left-in-place",
			"the cluster's Hetzner Cloud resources are deleted, but the dedicated server and its vSwitch were left in place: %s", refused.StatusDetail)
	case ghErr != nil:
		return nil, handler.Refuse("repository-left-in-place",
			"the cluster's Hetzner resources are deleted and the dedicated server is released, but the cluster repository was left in place: %s",
			refusalDetail(ghErr))
	}
	return nil, deleteMarkedRepository(ctx, gh, d)
}

func dialClusterRepository(d contract.Desired) (*githubAccess, error) {
	appName := d.Params["appName"]
	if appName == "" {
		return nil, handler.Refuse("missing-param", `the app name is required to name the cluster repository: params["appName"] is empty`)
	}
	return dialGitHub(d, appName)
}

// deleteMarkedRepository reads the mark just before the delete, and keeps a repository without it untouched.
func deleteMarkedRepository(ctx context.Context, gh *githubAccess, d contract.Desired) error {
	repo, err := gh.readRepo(ctx, d.SubscriptionID)
	if err != nil {
		return err
	}
	switch {
	case !repo.exists:
		return nil
	case !repo.marked:
		log.Print(safe.LogSafe(fmt.Sprintf("%s: subscription %s: kept the repository %s, which does not carry this subscription's mark",
			blockID, d.SubscriptionID, gh.repo.fullName()), requestSecrets(d)...))
		return nil
	}
	return gh.deleteRepo(ctx)
}

func requestSecrets(d contract.Desired) []string {
	var out []string
	for _, group := range d.Secrets {
		for _, v := range group {
			out = append(out, v)
		}
	}
	for _, v := range d.SecretInputs {
		out = append(out, v)
	}
	return out
}
