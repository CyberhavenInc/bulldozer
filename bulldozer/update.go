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

package bulldozer

import (
	"context"
	"time"

	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/CyberhavenInc/bulldozer/pull"
)

type rebaseUpdateCallback func(string)

const failThresholdMinutes = 60

var failedRebases map[int]time.Time

func RemoveFailedPR(prNumber int) {
	if failedRebases != nil {
		delete(failedRebases, prNumber)
	}
}

func ShouldUpdatePR(ctx context.Context, pullCtx pull.Context, updateConfig UpdateConfig) (bool, error) {
	logger := zerolog.Ctx(ctx)

	if !updateConfig.Blacklist.Enabled() && !updateConfig.Whitelist.Enabled() {
		return false, nil
	}

	if updateConfig.Blacklist.Enabled() {
		blacklisted, reason, err := IsPRBlacklisted(ctx, pullCtx, updateConfig.Blacklist)
		if err != nil {
			return false, errors.Wrap(err, "failed to determine if pull request is blacklisted")
		}
		if blacklisted {
			logger.Debug().Msgf("%s is deemed not updateable because blacklisting is enabled and %s", pullCtx.Locator(), reason)
			return false, nil
		}
	}

	if updateConfig.Whitelist.Enabled() {
		whitelisted, reason, err := IsPRWhitelisted(ctx, pullCtx, updateConfig.Whitelist)
		if err != nil {
			return false, errors.Wrap(err, "failed to determine if pull request is whitelisted")
		}
		if !whitelisted {
			logger.Debug().Msgf("%s is deemed not updateable because whitelisting is enabled and no whitelist signal detected", pullCtx.Locator())
			return false, nil
		}

		logger.Debug().Msgf("%s is whitelisted because whitelisting is enabled and %s", pullCtx.Locator(), reason)
	}

	return true, nil
}

func IsPRBehindBase(ctx context.Context, client *github.Client, pullCtx pull.Context) (bool, error) {
	logger := zerolog.Ctx(ctx)

	pr, _, err := client.PullRequests.Get(ctx, pullCtx.Owner(), pullCtx.Repo(), pullCtx.Number())
	if err != nil {
		logger.Error().Err(errors.WithStack(err)).Msgf("Failed to retrieve pull request %q", pullCtx.Locator())
		return false, err
	}

	if pr.GetState() == "closed" {
		return false, nil
	}

	if !pr.GetMergeable() && pr.GetMergeableState() != "unknown" {
		return false, nil
	}

	if pr.Head.Repo.GetFork() {
		return false, nil
	}

	baseRef := pr.GetBase().GetRef()
	comparison, _, err := client.Repositories.CompareCommits(ctx, pullCtx.Owner(), pullCtx.Repo(), baseRef, pr.GetHead().GetSHA())
	if err != nil {
		logger.Error().Err(errors.WithStack(err)).Msgf("cannot compare %s and %s for %q", baseRef, pr.GetHead().GetSHA(), pullCtx.Locator())
		return false, err
	}

	return comparison.GetBehindBy() > 0, nil
}

func UpdatePR(ctx context.Context, pullCtx pull.Context, client *github.Client, updateConfig UpdateConfig, baseRef string, onSuccess rebaseUpdateCallback) error {
	logger := zerolog.Ctx(ctx)

	//todo: should the updateConfig struct provide any other details here?

	go func(ctx context.Context, baseRef string) {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for i := 0; i < MaxPullRequestPollCount; i++ {
			<-ticker.C

			pr, _, err := client.PullRequests.Get(ctx, pullCtx.Owner(), pullCtx.Repo(), pullCtx.Number())
			if err != nil {
				logger.Error().Err(errors.WithStack(err)).Msgf("Failed to retrieve pull request %q", pullCtx.Locator())
				return
			}

			if pr.GetState() == "closed" {
				logger.Debug().Msg("Pull request already closed")
				return
			}

			if !pr.GetMergeable() {
				logger.Debug().Msg("Pull request is not in mergeable state")
				return
			}

			if pr.Head.Repo.GetFork() {
				logger.Debug().Msg("Pull request is from a fork, cannot keep it up to date with base ref")
				return
			}

			comparison, _, err := client.Repositories.CompareCommits(ctx, pullCtx.Owner(), pullCtx.Repo(), baseRef, pr.GetHead().GetSHA())
			if err != nil {
				logger.Error().Err(errors.WithStack(err)).Msgf("cannot compare %s and %s for %q", baseRef, pr.GetHead().GetSHA(), pullCtx.Locator())
			}
			if comparison.GetBehindBy() > 0 {
				logger.Debug().Msg("Pull request is not up to date")

				if failedRebases == nil {
					failedRebases = make(map[int]time.Time)
				}

				// Don't try to rebase if last rebase failed recently
				now := time.Now().UTC()
				if lastFail, has := failedRebases[pr.GetNumber()]; has {
					diff := now.Sub(lastFail)
					if diff.Minutes() < failThresholdMinutes {
						logger.Info().Msgf("PR rebase has failed %v ago, aborting rebase", diff)
						return
					}
				}

				h := RebaseHandler{
					ctx:    ctx,
					client: client,
					owner:  pullCtx.Owner(),
					repo:   pullCtx.Repo(),
				}

				if locked, err := h.interlockedRebase(pr); err != nil {
					logger.Error().Err(errors.WithStack(err)).Msgf("Failed to rebase pull request %q", pullCtx.Locator())
					failedRebases[pr.GetNumber()] = now
				} else if locked {
					logger.Info().Msgf("Pull request %q is already locked, skipping", pullCtx.Locator())
				} else {
					onSuccess(pullCtx.Locator())
					logger.Info().Msgf("Successfully updated pull %q request from base ref %s as rebase", pullCtx.Locator(), baseRef)
				}
			} else {
				logger.Debug().Msg("Pull request is not out of date, not updating")
			}

			return
		}
	}(zerolog.Ctx(ctx).WithContext(context.Background()), baseRef)

	return nil
}
