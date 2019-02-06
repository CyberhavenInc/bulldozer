package bulldozer

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/google/go-github/github"
	"github.com/nu7hatch/gouuid"
)

const (
	refsPrefix   = "refs/"
	branchPrefix = "heads/"

	stateUnlocked uint32 = iota
	stateLocked
)

var prLock = stateUnlocked

type withTmpRefFn func(tmpRef *string) error

type RebaseHandler struct {
	ctx    context.Context
	client *github.Client
	owner  string
	repo   string
}

func makeHeadsRef(ref string) string {
	ref = strings.TrimPrefix(ref, refsPrefix)
	ref = strings.TrimPrefix(ref, branchPrefix)
	return branchPrefix + ref
}

func (h *RebaseHandler) withTmpRef(headSHA string, function withTmpRefFn) error {
	u, err := uuid.NewV4()
	if err != nil {
		return err
	}

	tmpRefName := makeHeadsRef("tmp/rebase-" + u.String())
	refData := github.Reference{Ref: &tmpRefName, Object: &github.GitObject{SHA: &headSHA}}
	tmpRef, _, err := h.client.Git.CreateRef(h.ctx, h.owner, h.repo, &refData)
	if err != nil {
		return err
	}

	// Always delete tmp ref
	defer h.client.Git.DeleteRef(h.ctx, h.owner, h.repo, tmpRef.GetRef())

	return function(tmpRef.Ref)
}

func (h *RebaseHandler) createSiblingCommit(ref *string, tree *github.Tree, commit *github.RepositoryCommit) error {
	message := "sibling of " + *commit.SHA
	siblingData := github.Commit{
		Author:    commit.GetCommit().GetAuthor(),
		Committer: commit.GetCommit().GetCommitter(),
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

func (h *RebaseHandler) cherryPickCommit(ref, headSHA *string, tree *github.Tree, commit *github.RepositoryCommit) (newHeadSHA *string, newTree *github.Tree, err error) {
	if err := h.createSiblingCommit(ref, tree, commit); err != nil {
		return nil, nil, err
	}

	// Merge original commit into tmp branch.
	mergeReq := github.RepositoryMergeRequest{Base: ref, Head: commit.SHA}
	mergeCommit, _, err := h.client.Repositories.Merge(h.ctx, h.owner, h.repo, &mergeReq)
	if err != nil {
		return
	}
	newTree = mergeCommit.GetCommit().Tree

	// Create commit with different tree.
	commitData := github.Commit{
		Author:    commit.GetCommit().GetAuthor(),
		Committer: commit.GetCommit().GetCommitter(),
		Message:   commit.GetCommit().Message,
		Tree:      newTree,
		Parents:   []github.Commit{github.Commit{SHA: headSHA}},
	}

	newHeadCommit, _, err := h.client.Git.CreateCommit(h.ctx, h.owner, h.repo, &commitData)
	if err != nil {
		return
	}
	newHeadSHA = newHeadCommit.SHA

	// Overwrite the merge commit and its parent on the branch by a single commit.
	// The result will be equivalent to what would have happened with a fast-forward merge.
	newRefData := github.Reference{Ref: ref, Object: &github.GitObject{SHA: newHeadCommit.SHA}}
	_, _, err = h.client.Git.UpdateRef(h.ctx, h.owner, h.repo, &newRefData, true)
	return
}

func (h *RebaseHandler) cherryPickCommitsOnRef(ref, headSHA *string, tree *github.Tree, commits []*github.RepositoryCommit) (newHeadSHA *string, err error) {
	newHeadSHA = headSHA
	newTree := tree
	for _, commit := range commits {
		if newHeadSHA, newTree, err = h.cherryPickCommit(ref, newHeadSHA, newTree, commit); err != nil {
			return
		}
	}

	// Validate fast-forward of tmp branch.
	newRefData := github.Reference{Ref: ref, Object: &github.GitObject{SHA: newHeadSHA}}
	_, _, err = h.client.Git.UpdateRef(h.ctx, h.owner, h.repo, &newRefData, false)
	return
}

func (h *RebaseHandler) checkSameHead(ref, initialSHA string) error {
	actualRef, _, err := h.client.Git.GetRef(h.ctx, h.owner, h.repo, makeHeadsRef(ref))
	if err != nil {
		return err
	}

	if initialSHA != *actualRef.Object.SHA {
		return errors.New("current ref SHA doesn't match original ref SHA")
	}

	return nil
}

func (h *RebaseHandler) rebase(pr *github.PullRequest) error {
	baseRef, _, err := h.client.Git.GetRef(h.ctx, h.owner, h.repo, makeHeadsRef(pr.GetBase().GetRef()))
	if err != nil {
		return err
	}

	baseCommit, _, err := h.client.Git.GetCommit(h.ctx, h.owner, h.repo, baseRef.GetObject().GetSHA())
	if err != nil {
		return err
	}

	prCommits, _, err := h.client.PullRequests.ListCommits(h.ctx, h.owner, h.repo, pr.GetNumber(), &github.ListOptions{})
	if err != nil {
		return err
	}

	return h.withTmpRef(*baseRef.Object.SHA, func(tmpRef *string) error {
		headRef := pr.GetHead().GetRef()

		// Cherry-pick PR commits onto tmp ref
		var newHeadSHA *string
		if newHeadSHA, err = h.cherryPickCommitsOnRef(tmpRef, baseCommit.SHA, baseCommit.Tree, prCommits); err != nil {
			return err
		}

		// Ensure PR head didn't change while merging was in progress
		if err := h.checkSameHead(headRef, pr.GetHead().GetSHA()); err != nil {
			return err
		}

		// Update PR ref
		prRef := makeHeadsRef(headRef)
		newRefData := github.Reference{Ref: &prRef, Object: &github.GitObject{SHA: newHeadSHA}}
		_, _, err = h.client.Git.UpdateRef(h.ctx, h.owner, h.repo, &newRefData, true)
		return err
	})
}

func (h *RebaseHandler) interlockedRebase(pr *github.PullRequest) error {
	if !atomic.CompareAndSwapUint32(&prLock, stateUnlocked, stateLocked) {
		return errors.New("PR already locked")
	}
	defer atomic.StoreUint32(&prLock, stateUnlocked)

	return h.rebase(pr)
}
