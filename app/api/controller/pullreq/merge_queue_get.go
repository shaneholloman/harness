// Copyright 2023 Harness, Inc.
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

package pullreq

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/harness/gitness/app/api/usererror"
	"github.com/harness/gitness/app/auth"
	"github.com/harness/gitness/app/services/protection"
	"github.com/harness/gitness/errors"
	"github.com/harness/gitness/git/sha"
	"github.com/harness/gitness/store"
	"github.com/harness/gitness/types"
	"github.com/harness/gitness/types/enum"
)

type MergeQueueGetOutput struct {
	State             enum.MergeQueueEntryState `json:"state"`
	MergeCommitSHA    sha.SHA                   `json:"merge_commit_sha"`
	ChecksCommitSHA   sha.SHA                   `json:"checks_commit_sha"`
	Checks            []types.PullReqCheck      `json:"checks"`
	PullRequestsAhead int                       `json:"pull_requests_ahead"`
}

func (c *Controller) MergeQueueGet(
	ctx context.Context,
	session *auth.Session,
	repoRef string,
	pullreqNum int64,
) (*MergeQueueGetOutput, error) {
	targetRepo, err := c.getRepoCheckAccess(ctx, session, repoRef, enum.PermissionRepoView)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire access to target repo: %w", err)
	}

	pr, err := c.pullreqStore.FindByNumber(ctx, targetRepo.ID, pullreqNum)
	if err != nil {
		return nil, fmt.Errorf("failed to get pull request by number: %w", err)
	}

	if pr.State != enum.PullReqStateOpen || pr.SubState != enum.PullReqSubStateMergeQueue {
		return nil, usererror.BadRequest("Pull request is not in merge queue.")
	}

	entry, err := c.mergeQueueEntryStore.Find(ctx, pr.ID)
	if errors.Is(err, store.ErrResourceNotFound) {
		return nil, usererror.BadRequest("Pull request is not in merge queue.")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to find merge queue entry: %w", err)
	}

	entries, err := c.mergeQueueEntryStore.ListForMergeQueue(ctx, entry.MergeQueueID)
	if err != nil {
		return nil, fmt.Errorf("failed to list merge queue entries: %w", err)
	}

	var prsAhead int
	var found bool
	for _, e := range entries {
		if e.PullReqID == pr.ID {
			found = true
			break
		}
		prsAhead++
	}
	if !found {
		return nil, usererror.BadRequest("Pull request is not in merge queue.")
	}

	protectionRules, _, err := c.fetchRules(ctx, session, targetRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch rules: %w", err)
	}

	setup, err := protectionRules.GetMergeQueueSetup(protection.MergeQueueSetupInput{
		Repo:         targetRepo,
		TargetBranch: pr.TargetBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get merge queue setup: %w", err)
	}

	requiredIDs := make(map[string]struct{}, len(setup.RequiredChecks))
	for _, id := range setup.RequiredChecks {
		requiredIDs[id] = struct{}{}
	}

	prChecks := make([]types.PullReqCheck, 0, len(requiredIDs))

	if !entry.ChecksCommitSHA.IsEmpty() && !entry.ChecksCommitSHA.IsNil() {
		checks, err := c.mergeQueueService.ListChecks(ctx, targetRepo.ID, entry.ChecksCommitSHA)
		if err != nil {
			return nil, fmt.Errorf("failed to list checks: %w", err)
		}

		for _, check := range checks {
			_, required := requiredIDs[check.Identifier]
			if required {
				delete(requiredIDs, check.Identifier)
			}

			prChecks = append(prChecks, types.PullReqCheck{
				Required:   required,
				Bypassable: false,
				Check:      check,
			})
		}
	}

	for requiredID := range requiredIDs {
		prChecks = append(prChecks, types.PullReqCheck{
			Required:   true,
			Bypassable: false,
			Check: types.Check{
				RepoID:     targetRepo.ID,
				CommitSHA:  entry.ChecksCommitSHA.String(),
				Identifier: requiredID,
				Status:     enum.CheckStatusPending,
				Metadata:   json.RawMessage("{}"),
			},
		})
	}

	sort.Slice(prChecks, func(i, j int) bool {
		return prChecks[i].Check.Identifier < prChecks[j].Check.Identifier
	})

	return &MergeQueueGetOutput{
		State:             entry.State,
		MergeCommitSHA:    entry.MergeCommitSHA,
		ChecksCommitSHA:   entry.ChecksCommitSHA,
		Checks:            prChecks,
		PullRequestsAhead: prsAhead,
	}, nil
}
