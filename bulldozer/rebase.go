package bulldozer

import (
	"context"
	"strings"

	"github.com/google/go-github/github"
)

const (
	branchPrefix = "heads/"
	lockLabel    = "Auto update"
)

type withTmpRefFn func(tmpRef *string) error
type withLabelLockFn func(pr *github.PullRequest) error

type RebaseHandler struct {
	ctx    context.Context
	client *github.Client
	owner  string
	repo   string
}

func makeHeadRef(ref string) string {
	if strings.HasPrefix(ref, branchPrefix) {
		return ref
	}
	return branchPrefix + ref
}

func (h *RebaseHandler) withLabelLock(pr *github.PullRequest, function withLabelLockFn) error {
	// We need to lock PR so no other thread can perform another rebase.
	// We try to remove label from PR. If this succedes, we have a lock and can continue.
	// API fails if there is no such label and this means someone else has a lock.
	if _, err := h.client.Issues.RemoveLabelForIssue(h.ctx, h.owner, h.repo, *pr.Number, lockLabel); err != nil {
		return err
	}

	funcErr := function(pr)

	if _, _, err := h.client.Issues.AddLabelsToIssue(h.ctx, h.owner, h.repo, *pr.Number, []string{lockLabel}); err != nil {
		return err
	}
	return funcErr
}

func (h *RebaseHandler) withTmpRef(headSHA string, function withTmpRefFn) error {
	tmpRefName := makeHeadRef("issue/DAT-XXX-tmp-branch-test")
	refData := github.Reference{Ref: &tmpRefName, Object: &github.GitObject{SHA: &headSHA}}
	tmpRef, _, err := h.client.Git.CreateRef(h.ctx, h.owner, h.repo, &refData)
	if err != nil {
		return err
	}

	funcErr := function(tmpRef.Ref)

	// Always delete ref
	if _, err = h.client.Git.DeleteRef(h.ctx, h.owner, h.repo, *tmpRef.Ref); err != nil {
		return err
	}

	return funcErr
}

func (h *RebaseHandler) createSiblingCommit(ref *string, tree *github.Tree, commit *github.RepositoryCommit) error {
	message := "sibling of " + *commit.SHA
	siblingData := github.Commit{
		Author:    commit.Commit.Author,
		Committer: commit.Commit.Committer,
		Message:   &message,
		Parents:   commit.Parents,
		Tree:      tree,
	}
	sibling, _, err := h.client.Git.CreateCommit(h.ctx, h.owner, h.repo, &siblingData)
	if err != nil {
		return err
	}

	// Update tmp branch to sibling commit.
	refData := github.Reference{Ref: ref, Object: &github.GitObject{SHA: sibling.SHA}}
	if _, _, err = h.client.Git.UpdateRef(h.ctx, h.owner, h.repo, &refData, true); err != nil {
		return err
	}

	return nil
}

func (h *RebaseHandler) cherryPickCommit(ref, headSHA *string, tree *github.Tree, commit *github.RepositoryCommit) (*string, *github.Tree, error) {
	err := h.createSiblingCommit(ref, tree, commit)
	if err != nil {
		return nil, nil, err
	}

	// Merge original commit into tmp branch.
	mergeReq := github.RepositoryMergeRequest{Base: ref, Head: commit.SHA}
	mergeCommit, _, err := h.client.Repositories.Merge(h.ctx, h.owner, h.repo, &mergeReq)
	if err != nil {
		return nil, nil, err
	}

	// Create commit with different tree.
	commitData := github.Commit{
		Author:    commit.Commit.Author,
		Committer: commit.Commit.Committer,
		Message:   commit.Commit.Message,
		Parents:   []github.Commit{github.Commit{SHA: headSHA}},
		Tree:      mergeCommit.Commit.Tree,
	}
	newHeadCommit, _, err := h.client.Git.CreateCommit(h.ctx, h.owner, h.repo, &commitData)
	if err != nil {
		return nil, nil, err
	}

	// Overwrite the merge commit and its parent on the branch by a single commit.
	// The result will be equivalent to what would have happened with a fast-forward merge.
	newRefData := github.Reference{Ref: ref, Object: &github.GitObject{SHA: newHeadCommit.SHA}}
	if _, _, err = h.client.Git.UpdateRef(h.ctx, h.owner, h.repo, &newRefData, true); err != nil {
		return nil, nil, err
	}

	return newHeadCommit.SHA, mergeCommit.Commit.Tree, nil
}

func (h *RebaseHandler) cherryPickCommitsOnRef(ref, headSHA *string, tree *github.Tree, commits []*github.RepositoryCommit) (*string, error) {
	newHeadSHA := headSHA
	newTree := tree
	var err error

	for _, commit := range commits {
		if newHeadSHA, newTree, err = h.cherryPickCommit(ref, newHeadSHA, newTree, commit); err != nil {
			return nil, err
		}
	}

	// Validate fast-forward of tmp ref
	newRefData := github.Reference{Ref: ref, Object: &github.GitObject{SHA: newHeadSHA}}
	if _, _, err = h.client.Git.UpdateRef(h.ctx, h.owner, h.repo, &newRefData, false); err != nil {
		return nil, err
	}

	return newHeadSHA, nil
}

func (h *RebaseHandler) rebase(pr *github.PullRequest) error {
	baseRef, _, err := h.client.Git.GetRef(h.ctx, h.owner, h.repo, makeHeadRef(*pr.Base.Ref))
	if err != nil {
		return err
	}

	baseCommit, _, err := h.client.Git.GetCommit(h.ctx, h.owner, h.repo, *baseRef.Object.SHA)
	if err != nil {
		return err
	}

	prCommits, _, err := h.client.PullRequests.ListCommits(h.ctx, h.owner, h.repo, *pr.Number, &github.ListOptions{})
	if err != nil {
		return err
	}

	return h.withTmpRef(*baseRef.Object.SHA, func(tmpRef *string) error {
		// Cherry-pick PR commits onto tmp ref
		var newHeadSHA *string
		if newHeadSHA, err = h.cherryPickCommitsOnRef(tmpRef, baseCommit.SHA, baseCommit.Tree, prCommits); err != nil {
			return err
		}

		// Update PR ref
		prRef := makeHeadRef(*pr.Head.Ref)
		newRefData := github.Reference{Ref: &prRef, Object: &github.GitObject{SHA: newHeadSHA}}
		_, _, err = h.client.Git.UpdateRef(h.ctx, h.owner, h.repo, &newRefData, true)

		return err
	})
}

func (h *RebaseHandler) interlockedRebase(pr *github.PullRequest) error {
	return h.withLabelLock(pr, h.rebase)
}
