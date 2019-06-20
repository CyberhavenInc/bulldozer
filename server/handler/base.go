// Copyright 2018 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	"sort"

	"github.com/google/go-github/github"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/CyberhavenInc/bulldozer/bulldozer"
	"github.com/CyberhavenInc/bulldozer/pull"
)

type Base struct {
	githubapp.ClientCreator
	bulldozer.ConfigFetcher
}

type pullWithConfig struct {
	pr         *github.PullRequest
	pullCtx    pull.Context
	pullConfig bulldozer.Config
}

func (b *Base) ProcessPullRequest(ctx context.Context, pullCtx pull.Context, client *github.Client, pr *github.PullRequest) error {
	logger := zerolog.Ctx(ctx)

	bulldozerConfig, err := b.ConfigForPR(ctx, client, pr)
	if err != nil {
		return errors.Wrap(err, "failed to fetch configuration")
	}

	switch {
	case bulldozerConfig.Missing():
		logger.Debug().Msgf("No bulldozer configuration for %q", bulldozerConfig.String())
	case bulldozerConfig.Invalid():
		logger.Debug().Msgf("Bulldozer configuration is invalid for %q", bulldozerConfig.String())
	default:
		logger.Debug().Msgf("Bulldozer configuration is valid for %q", bulldozerConfig.String())
		config := *bulldozerConfig.Config
		shouldMerge, err := bulldozer.ShouldMergePR(ctx, pullCtx, config.Merge)
		if err != nil {
			return errors.Wrap(err, "unable to determine merge status")
		}
		if shouldMerge {
			logger.Debug().Msg("Pull request should be merged")
			if err := bulldozer.MergePR(ctx, pullCtx, client, config.Merge); err != nil {
				return errors.Wrap(err, "failed to merge pull request")
			}
		}
	}

	return nil
}

func (b *Base) UpdatePullRequest(ctx context.Context, pullCtx pull.Context, client *github.Client, pr *github.PullRequest, baseRef string) error {
	logger := zerolog.Ctx(ctx)

	bulldozerConfig, err := b.ConfigForPR(ctx, client, pr)
	if err != nil {
		return errors.Wrap(err, "failed to fetch configuration")
	}

	switch {
	case bulldozerConfig.Missing():
		logger.Debug().Msgf("No bulldozer configuration for %q", bulldozerConfig.String())
	case bulldozerConfig.Invalid():
		logger.Debug().Msgf("Bulldozer configuration is invalid for %q", bulldozerConfig.String())
	default:
		logger.Debug().Msgf("Bulldozer configuration is valid for %q", bulldozerConfig.String())
		config := *bulldozerConfig.Config

		shouldUpdate, err := bulldozer.ShouldUpdatePR(ctx, pullCtx, config.Update)

		if err != nil {
			return errors.Wrap(err, "unable to determine update status")
		}

		if shouldUpdate {
			logger.Debug().Msg("Pull request should be updated")
			if err := bulldozer.UpdatePR(ctx, pullCtx, client, config.Update, baseRef, AddActivePR); err != nil {
				return errors.Wrap(err, "failed to update pull request")
			}
		}
	}

	return nil
}

func (b *Base) FilterUpdatablePRs(ctx context.Context, client *github.Client, prs []*github.PullRequest) (result []pullWithConfig) {
	logger := zerolog.Ctx(ctx)

	for _, pr := range prs {
		bulldozerConfig, err := b.ConfigForPR(ctx, client, pr)
		if err != nil {
			logger.Debug().Msgf("unable to fetch config for pr: %v", err)
			continue
		}

		if bulldozerConfig.Missing() || bulldozerConfig.Invalid() {
			logger.Debug().Msgf("Invalid bulldozer configuration for %q", bulldozerConfig.String())
			continue
		}

		config := *bulldozerConfig.Config
		pullCtx := pull.NewGithubContext(client, pr, bulldozerConfig.Owner, bulldozerConfig.Repo, pr.GetNumber())

		canUpdate, err := bulldozer.ShouldUpdatePR(ctx, pullCtx, config.Update)
		if err != nil {
			logger.Debug().Msgf("unable to determine whitelist status: %v", err)
			continue
		}

		behindBase, err := bulldozer.IsPRBehindBase(ctx, client, pullCtx)
		if err != nil {
			logger.Debug().Msgf("unable to determine update status: %v", err)
			continue
		}

		if canUpdate && behindBase {
			result = append(result, pullWithConfig{pr: pr, pullCtx: pullCtx, pullConfig: config})
		}
	}

	return
}

func (b *Base) UpdateOldestPullRequest(ctx context.Context, client *github.Client, prs []pullWithConfig) error {
	if len(prs) == 0 {
		return nil
	}

	// Other PRs are being updated
	if ActivePRPresent() {
		return nil
	}

	// Sort by creation time, oldest first
	sort.Slice(prs, func(i, j int) bool {
		return prs[i].pr.GetCreatedAt().Before(prs[j].pr.GetCreatedAt())
	})

	oldest := prs[0]
	baseRef := oldest.pr.GetBase().GetRef()
	if err := bulldozer.UpdatePR(ctx, oldest.pullCtx, client, oldest.pullConfig.Update, baseRef, AddActivePR); err != nil {
		return errors.Wrap(err, "failed to update pull request")
	}

	return nil
}
