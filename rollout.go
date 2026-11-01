package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

// rollOut commits each release file the repository does not hold as stated, for Argo CD to apply, since Talos never
// re-applies a manifest it created.
func (g *githubAccess) rollOut(ctx context.Context, release rollout.Release) error {
	files, err := release.Files(g.repo.fullName())
	if err != nil {
		return err
	}
	for _, f := range files {
		held, found, err := g.readFile(ctx, f.Path)
		if err != nil {
			return err
		}
		if found && held == string(f.Bytes) {
			continue
		}
		if err := g.putFile(ctx, f.Path, string(f.Bytes)); err != nil {
			return err
		}
	}
	return nil
}

// readBack judges only a repository holding the release's files, so a cluster on an earlier release reads as before
// until an upgrade.
func (g *githubAccess) readBack(ctx context.Context, d contract.Desired, release rollout.Release) (applied, problem string, err error) {
	files, err := release.Files(g.repo.fullName())
	if err != nil {
		return "", "", err
	}
	for _, f := range files {
		held, found, err := g.readFile(ctx, f.Path)
		if err != nil || !found || held != string(f.Bytes) {
			return "", "", err
		}
	}
	reports, err := nodeReports.NodeReports(ctx, d, d.SubscriptionID)
	if err != nil {
		return "", fmt.Sprintf("the node reports cannot be read to read the hosting policies back: %v", err), nil
	}
	newest := newestReport(reports, time.Time{})
	if newest == nil {
		return "", "the cluster has sent no node report to read the hosting policies back from", nil
	}
	reading, err := release.ReadBack(newest.Body)
	if err != nil {
		return "", "", err
	}
	if reading.Problem != "" {
		return "", reading.Problem, nil
	}
	return fmt.Sprintf("Argo CD's Application %s holds %s", rollout.Application, strings.Join(reading.Applied, ", ")), "", nil
}
