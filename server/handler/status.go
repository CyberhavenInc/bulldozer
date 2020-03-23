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
	"encoding/json"

	"github.com/google/go-github/github"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/CyberhavenInc/bulldozer/pull"
)

type Status struct {
	Base
}

func (h *Status) Handles() []string {
	return []string{"status"}
}

func (h *Status) Handle(ctx context.Context, eventType, deliveryID string, payload []byte) error {
	var event github.StatusEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return errors.Wrap(err, "failed to parse status event payload")
	}

	repo := event.GetRepo()
	owner := repo.GetOwner().GetLogin()
	repoName := repo.GetName()
	installationID := githubapp.GetInstallationIDFromEvent(&event)
	ctx, logger := githubapp.PrepareRepoContext(ctx, installationID, repo)
	state := event.GetState()
	eventStatusName := event.GetContext()

	if state == "pending" {
		logger.Debug().Msgf("Doing nothing since context state for %q was %q", eventStatusName, event.GetState())
		return nil
	}

	client, err := h.ClientCreator.NewInstallationClient(installationID)
	if err != nil {
		return errors.Wrap(err, "failed to instantiate github client")
	}

	prs, err := pull.ListOpenPullRequestsForSHA(ctx, client, owner, repoName, event.GetSHA())
	if err != nil {
		return errors.Wrap(err, "failed to determine open pull requests matching the status context change")
	}

	required := false
	wasActive := false
	for _, pr := range prs {
		pullCtx := pull.NewGithubContext(client, pr, owner, repoName, pr.GetNumber())
		required = h.isStatusRequired(ctx, pullCtx, eventStatusName)

		// Cleanup PR state
		wasActive = wasActive || RemoveActivePR(pullCtx.Locator())
	}

	// Detect failure in recently rebased PR and schedule another rebase
	if state == "error" || state == "failure" {
		if required && (wasActive || !ActivePRPresent()) {
			if err := h.tryUpdateAnotherPR(logger.WithContext(ctx), client, event); err != nil {
				logger.Error().Err(errors.WithStack(err)).Msg("Failed to update another pull request")
			}
			return nil
		}
	} else if state != "success" {
		logger.Error().Msgf("Unexpected state for %q: %q", event.GetContext(), event.GetState())
		return nil
	}

	if len(prs) == 0 {
		logger.Debug().Msg("Doing nothing since status change event affects no open pull requests")
		return nil
	}

	// PR became outdated while building, reschedule update again
	stillBehindBase := h.FilterUpdatablePRs(ctx, client, prs)
	if len(stillBehindBase) > 0 {
		if err := h.tryUpdateAnotherPR(logger.WithContext(ctx), client, event); err != nil {
			logger.Error().Err(errors.WithStack(err)).Msg("Failed to update another pull request")
		}
		return nil
	}

	for _, pr := range prs {
		pullCtx := pull.NewGithubContext(client, pr, owner, repoName, pr.GetNumber())
		logger := logger.With().Int(githubapp.LogKeyPRNum, pr.GetNumber()).Logger()

		if err := h.ProcessPullRequest(logger.WithContext(ctx), pullCtx, client, pr); err != nil {
			logger.Error().Err(errors.WithStack(err)).Msg("Error processing pull request")
		}
	}

	return nil
}

func (h *Status) tryUpdateAnotherPR(ctx context.Context, client *github.Client, event github.StatusEvent) error {
	repo := event.GetRepo()
	owner := repo.GetOwner().GetLogin()
	repoName := repo.GetName()

	prs, err := pull.ListOpenPullRequests(ctx, client, owner, repoName)
	if err != nil {
		return err
	}

	filtered := h.FilterUpdatablePRs(ctx, client, prs)
	if len(filtered) == 0 {
		return nil
	}

	if err := h.UpdateOldestPullRequest(ctx, client, filtered); err != nil {
		return err
	}

	return nil
}

func (h *Status) isStatusRequired(ctx context.Context, pullCtx pull.Context, eventStatusName string) bool {
	// Check if status of the event is manadatory for the merge
	if requiredStatuses, err := pullCtx.RequiredStatuses(ctx); err == nil {
		for _, name := range requiredStatuses {
			if eventStatusName == name {
				return true
			}
		}
	} else {
		zerolog.Ctx(ctx).Warn().Msgf("Failed to get required PR status list: %v", err)
	}
	return false
}

// type assertion
var _ githubapp.EventHandler = &Status{}
