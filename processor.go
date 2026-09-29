package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v52/github"
	"go.uber.org/zap"
)

// A stable branch name lets later runs update the open PR instead of opening a new one each time.
const pullRequestBranch = "filemaintainer/update"

// errTreeTruncated means the API can't list every existing file mode, so the git-based update,
// which preserves modes naturally, must be used instead.
var errTreeTruncated = errors.New("repository tree is too large to read through the API")

type Processor struct {
	dryRun   bool
	onlyRepo string
	gh       *github.Client
	resolver *RemoteResolver
	logger   *zap.SugaredLogger
}

type fileChange struct {
	source        string
	dest          string
	content       []byte
	exists        bool
	remoteContent string
}

func (c fileChange) verb() string {
	if c.exists {
		return "Update"
	}
	return "Create"
}

type repoChanges struct {
	owner   string
	repo    string
	changes []fileChange
}

func (rc *repoChanges) fullName() string {
	return rc.owner + "/" + rc.repo
}

// Shared by the commit subject and PR title so the PR and its squash-merged commit read the same.
const changeTitle = "FileMaintainer Update"

func (rc *repoChanges) body() string {
	lines := make([]string, 0, len(rc.changes))
	for _, c := range rc.changes {
		lines = append(lines, fmt.Sprintf("- %s %s", c.verb(), c.dest))
	}
	return strings.Join(lines, "\n")
}

func (rc *repoChanges) commitMessage() string {
	return changeTitle + "\n\n" + rc.body()
}

func NewProcessor(dryRun bool, onlyRepo string, gh *github.Client, logger *zap.SugaredLogger) *Processor {
	return &Processor{
		dryRun:   dryRun,
		onlyRepo: onlyRepo,
		gh:       gh,
		logger:   logger,
		resolver: NewRemoteResolver(gh, logger),
	}
}

// ProcessFiles keeps going past repository-specific failures so one broken repository doesn't
// block the rest, and reports them all at the end.
func (p *Processor) ProcessFiles(config Config) error {
	pending, failures, err := p.collectChanges(config)
	if err != nil {
		return err
	}

	for _, rc := range pending {
		err := p.applyChanges(rc, repoSettingsFor(config, rc.fullName()))
		if err != nil {
			err = fmt.Errorf("failed to apply changes to %s: %w", rc.fullName(), err)
			p.logger.Errorf("%s", err)
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// GitHub repository names are case-insensitive, so config keys must be matched the same way.
func repoSettingsFor(config Config, fullName string) RepoSettings {
	for name, settings := range config.Repo {
		if strings.EqualFold(name, fullName) {
			return settings
		}
	}
	return RepoSettings{}
}

// collectChanges gathers every file that needs to change, grouped by repository, so that each
// repository receives a single commit. Repositories that fail are returned as failures and left out
// entirely, since committing only some of their files would drop the rest from an open PR.
func (p *Processor) collectChanges(config Config) ([]*repoChanges, []error, error) {
	fileNames := make([]string, 0, len(config.File))
	for name := range config.File {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)

	changes := newChangeSet()
	failed := make(map[string]bool)
	failures := []error{}
	for _, fileName := range fileNames {
		file := config.File[fileName]
		msg := fmt.Sprintf("Processing file %s", file.Dest)
		sep := strings.Repeat("-", len(msg))
		p.logger.Infof(sep)
		p.logger.Infof(msg)
		p.logger.Infof(sep)

		content, err := os.ReadFile(file.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read file %s: %s", file.Path, err)
		}

		for _, remoteName := range file.Remotes {
			remote, ok := config.Remote[remoteName]
			if !ok {
				return nil, nil, fmt.Errorf("did not find a remote named %s in remotes", remoteName)
			}

			err = p.applyToAllRepos(remote, remoteName, func(owner string, repo string) error {
				// Remotes may spell the same repository with different case; splitting it into
				// separate groups would make each group's force-push discard the other's changes.
				key := strings.ToLower(owner + "/" + repo)
				if failed[key] {
					return nil
				}
				change, err := p.diffFile(owner, repo, file.Dest, content)
				if err == nil && change != nil {
					change.source = fileName
					err = changes.add(key, owner, repo, *change)
				}
				if err != nil {
					p.logger.Errorf("skipping %s/%s: %s", owner, repo, err)
					failed[key] = true
					failures = append(failures, err)
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		}
	}

	pending := make([]*repoChanges, 0, len(changes.order))
	for _, key := range changes.order {
		if !failed[key] {
			pending = append(pending, changes.byRepo[key])
		}
	}
	return pending, failures, nil
}

// changeSet groups changes by repository while preserving the order repositories were first seen,
// so runs are deterministic.
type changeSet struct {
	byRepo map[string]*repoChanges
	order  []string
}

func newChangeSet() *changeSet {
	return &changeSet{byRepo: make(map[string]*repoChanges)}
}

func (cs *changeSet) add(key string, owner string, repo string, change fileChange) error {
	rc, ok := cs.byRepo[key]
	if !ok {
		rc = &repoChanges{owner: owner, repo: repo}
		cs.byRepo[key] = rc
		cs.order = append(cs.order, key)
	}
	for _, existing := range rc.changes {
		if existing.dest != change.dest {
			continue
		}
		// Overlapping remotes can select the same repo for the same file more than once.
		if bytes.Equal(existing.content, change.content) {
			return nil
		}
		return fmt.Errorf("files %s and %s both write %s/%s with different content", existing.source, change.source, rc.fullName(), change.dest)
	}
	rc.changes = append(rc.changes, change)
	return nil
}

// diffFile returns the change needed to make dest in the repo's default branch match content, or
// nil if it already matches.
func (p *Processor) diffFile(owner string, repo string, dest string, content []byte) (*fileChange, error) {
	var remoteContentResp *github.RepositoryContent
	var resp *github.Response
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		remoteContentResp, _, resp, err = p.gh.Repositories.GetContents(context.Background(), owner, repo, dest, &github.RepositoryContentGetOptions{})
		if resp != nil && resp.StatusCode == 500 && attempt < 3 {
			backoff := time.Duration(attempt*attempt) * 500 * time.Millisecond
			p.logger.Debugf("received 500 getting contents of file %s/%s/%s (attempt %d/%d); retrying in %s: %s", owner, repo, dest, attempt, 3, backoff, err)
			time.Sleep(backoff)
			continue
		}
		break
	}
	if resp == nil {
		return nil, fmt.Errorf("failed to fetch contents of file %s/%s/%s: %s", owner, repo, dest, err)
	}

	switch resp.StatusCode {
	case 200:
		// Directories come back as a listing with no single file response.
		if remoteContentResp == nil || remoteContentResp.GetType() != "file" {
			return nil, fmt.Errorf("%s/%s/%s exists but is not a file", owner, repo, dest)
		}
		// GitHub omits the content of files over 1 MB, but always reports the blob SHA.
		if remoteContentResp.GetSHA() == gitBlobSHA(content) {
			p.logger.Debugf("skipping %s/%s/%s because it does not need to be updated", owner, repo, dest)
			return nil, nil
		}
		// Only feeds the dry-run summary, which can tolerate missing content.
		remoteContent, _ := remoteContentResp.GetContent()
		return &fileChange{dest: dest, content: content, exists: true, remoteContent: remoteContent}, nil
	case 404:
		return &fileChange{dest: dest, content: content}, nil
	default:
		return nil, fmt.Errorf("failed to fetch contents of file %s/%s/%s: %s", owner, repo, dest, err)
	}
}

func gitBlobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Processor) applyChanges(rc *repoChanges, settings RepoSettings) error {
	ctx := context.Background()
	repository, _, err := p.gh.Repositories.Get(ctx, rc.owner, rc.repo)
	if err != nil {
		return fmt.Errorf("failed to get repository: %s", err)
	}
	defaultBranch := repository.GetDefaultBranch()
	targetBranch := pullRequestBranch
	if settings.PushToDefaultBranch {
		targetBranch = defaultBranch
	}

	if p.dryRun {
		return p.printChangesDryRun(ctx, rc, defaultBranch, targetBranch)
	}

	pushed, apiErr := p.commitViaAPI(ctx, rc, defaultBranch, targetBranch)
	if shouldFallBackToGit(apiErr) {
		p.logger.Debugf("could not commit to %s via API (will try git-based update): %s", rc.fullName(), apiErr)
		var gitErr error
		pushed, gitErr = p.commitViaGit(rc, defaultBranch, targetBranch)
		if gitErr != nil {
			return fmt.Errorf("API-based update failed: %s; git-based update failed: %s", apiErr, gitErr)
		}
	} else if apiErr != nil {
		return apiErr
	}
	if pushed {
		p.logger.Infof("pushed %d file(s) to %s on branch %s", len(rc.changes), rc.fullName(), targetBranch)
	} else {
		p.logger.Infof("branch %s of %s is already up to date", targetBranch, rc.fullName())
	}

	if settings.PushToDefaultBranch {
		return nil
	}
	return p.ensurePullRequest(ctx, rc, defaultBranch)
}

func (p *Processor) printChangesDryRun(ctx context.Context, rc *repoChanges, baseBranch string, targetBranch string) error {
	for _, c := range rc.changes {
		if c.exists {
			p.printUpdateFileDryRun(c.remoteContent, string(c.content), rc.owner, rc.repo, c.dest)
		} else {
			p.logger.Infof("would create file %s/%s/%s", rc.owner, rc.repo, c.dest)
		}
	}

	push, err := p.wouldPush(ctx, rc, baseBranch, targetBranch)
	switch {
	case errors.Is(err, errTreeTruncated):
		p.logger.Infof("would push %d file(s) to branch %s of %s (repository is too large to check whether it is already up to date)", len(rc.changes), targetBranch, rc.fullName())
	case err != nil:
		return err
	case push:
		p.logger.Infof("would push %d file(s) to branch %s of %s", len(rc.changes), targetBranch, rc.fullName())
	default:
		p.logger.Infof("branch %s of %s is already up to date; would not push", targetBranch, rc.fullName())
	}

	if targetBranch == baseBranch {
		return nil
	}
	pr, err := p.findPullRequest(ctx, rc, baseBranch)
	if err != nil {
		return err
	}
	if pr == nil {
		p.logger.Infof("would open a pull request from %s to %s in %s", pullRequestBranch, baseBranch, rc.fullName())
	} else {
		p.logger.Infof("pull request %s is already open", pr.GetHTMLURL())
	}
	return nil
}

// wouldPush predicts commitViaAPI's decision without writing to the repository, by comparing every
// file the new commit would contain against the target branch.
func (p *Processor) wouldPush(ctx context.Context, rc *repoChanges, baseBranch string, targetBranch string) (bool, error) {
	branches, err := p.readBranches(ctx, rc, baseBranch, targetBranch)
	if err != nil || !branches.targetExists {
		return true, err
	}

	want, err := p.flatTree(ctx, rc, branches.baseTreeSHA)
	if err != nil {
		return true, err
	}
	for _, c := range rc.changes {
		want[c.dest] = treeLeaf{mode: treeEntryMode(want[c.dest].mode == executableMode), sha: gitBlobSHA(c.content)}
	}
	have, err := p.flatTree(ctx, rc, branches.targetTreeSHA)
	if err != nil {
		return true, err
	}
	return !maps.Equal(want, have), nil
}

func (p *Processor) printUpdateFileDryRun(remoteContent string, content string, owner string, repo string, dest string) {
	p.logger.Infof("would update %d lines in file %s/%s/%s", countChangedLines(remoteContent, content), owner, repo, dest)
}

// countChangedLines compares lines positionally, which is only a rough size estimate for dry runs.
func countChangedLines(before string, after string) int {
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")
	changed := 0
	for i := range max(len(beforeLines), len(afterLines)) {
		if i >= len(beforeLines) || i >= len(afterLines) || beforeLines[i] != afterLines[i] {
			changed++
		}
	}
	return changed
}

// A git-based update only helps when the API itself is the obstacle, e.g. a conflicting ref
// update. Auth and permission failures would fail the same way over git.
func shouldFallBackToGit(err error) bool {
	if errors.Is(err, errTreeTruncated) {
		return true
	}
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case 409, 422:
			return true
		}
	}
	return false
}

// commitViaAPI creates one commit containing every change on top of baseBranch and points
// targetBranch at it, reporting whether anything was pushed. A PR branch is force-updated because
// FileMaintainer owns it and rebuilds it from the current base on every run.
func (p *Processor) commitViaAPI(ctx context.Context, rc *repoChanges, baseBranch string, targetBranch string) (bool, error) {
	branches, err := p.readBranches(ctx, rc, baseBranch, targetBranch)
	if err != nil {
		return false, err
	}

	tree, err := p.createTree(ctx, rc, branches.baseTreeSHA)
	if err != nil {
		return false, err
	}

	// Skipping identical trees avoids re-pushing (and re-triggering CI on) an unchanged PR, and
	// avoids empty commits on the default branch.
	if branches.targetExists && branches.targetTreeSHA == tree.GetSHA() {
		return false, nil
	}

	msg := rc.commitMessage()
	commit, _, err := p.gh.Git.CreateCommit(ctx, rc.owner, rc.repo, &github.Commit{
		Message: &msg,
		Tree:    tree,
		Parents: []*github.Commit{{SHA: &branches.baseSHA}},
	})
	if err != nil {
		return false, fmt.Errorf("failed to create commit: %w", err)
	}

	targetRefName := "refs/heads/" + targetBranch
	newRef := &github.Reference{Ref: &targetRefName, Object: &github.GitObject{SHA: commit.SHA}}
	if !branches.targetExists {
		_, _, err = p.gh.Git.CreateRef(ctx, rc.owner, rc.repo, newRef)
	} else {
		_, _, err = p.gh.Git.UpdateRef(ctx, rc.owner, rc.repo, newRef, targetBranch != baseBranch)
	}
	if err != nil {
		return false, fmt.Errorf("failed to update branch %s: %w", targetBranch, err)
	}
	return true, nil
}

// branchState is what both committing and dry-run prediction need to know about the branches.
type branchState struct {
	baseSHA       string
	baseTreeSHA   string
	targetExists  bool
	targetTreeSHA string
}

func (p *Processor) readBranches(ctx context.Context, rc *repoChanges, baseBranch string, targetBranch string) (branchState, error) {
	baseSHA, baseTreeSHA, baseExists, err := p.readBranch(ctx, rc, baseBranch)
	if err != nil {
		return branchState{}, err
	}
	if !baseExists {
		return branchState{}, fmt.Errorf("branch %s does not exist", baseBranch)
	}
	state := branchState{baseSHA: baseSHA, baseTreeSHA: baseTreeSHA, targetExists: true, targetTreeSHA: baseTreeSHA}
	if targetBranch == baseBranch {
		return state, nil
	}

	_, state.targetTreeSHA, state.targetExists, err = p.readBranch(ctx, rc, targetBranch)
	return state, err
}

// readBranch returns the head commit and tree of branch, or exists=false if there is no such branch.
func (p *Processor) readBranch(ctx context.Context, rc *repoChanges, branch string) (sha string, treeSHA string, exists bool, err error) {
	ref, resp, err := p.gh.Git.GetRef(ctx, rc.owner, rc.repo, "refs/heads/"+branch)
	if resp != nil && resp.StatusCode == 404 {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("failed to get branch %s: %w", branch, err)
	}
	sha = ref.GetObject().GetSHA()
	commit, _, err := p.gh.Git.GetCommit(ctx, rc.owner, rc.repo, sha)
	if err != nil {
		return "", "", false, fmt.Errorf("failed to get commit %s: %w", sha, err)
	}
	return sha, commit.GetTree().GetSHA(), true, nil
}

func (p *Processor) createTree(ctx context.Context, rc *repoChanges, baseTreeSHA string) (*github.Tree, error) {
	executable, err := p.executablePaths(ctx, rc, baseTreeSHA)
	if err != nil {
		return nil, err
	}

	entries := make([]*github.TreeEntry, 0, len(rc.changes))
	for _, c := range rc.changes {
		// Blobs are uploaded as base64 so non-UTF-8 content survives intact.
		blob, _, err := p.gh.Git.CreateBlob(ctx, rc.owner, rc.repo, &github.Blob{
			Content:  github.String(base64.StdEncoding.EncodeToString(c.content)),
			Encoding: github.String("base64"),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create blob for %s: %w", c.dest, err)
		}
		entries = append(entries, &github.TreeEntry{
			Path: github.String(c.dest),
			Mode: github.String(treeEntryMode(executable[c.dest])),
			Type: github.String("blob"),
			SHA:  blob.SHA,
		})
	}

	tree, _, err := p.gh.Git.CreateTree(ctx, rc.owner, rc.repo, baseTreeSHA, entries)
	if err != nil {
		return nil, fmt.Errorf("failed to create tree: %w", err)
	}
	return tree, nil
}

// executablePaths finds which of the changed files are already executable, because the Git Data
// API requires an explicit mode for every entry and would otherwise clear the executable bit.
func (p *Processor) executablePaths(ctx context.Context, rc *repoChanges, baseTreeSHA string) (map[string]bool, error) {
	executable := make(map[string]bool)
	anyExisting := false
	for _, c := range rc.changes {
		anyExisting = anyExisting || c.exists
	}
	if !anyExisting {
		return executable, nil
	}

	leaves, err := p.flatTree(ctx, rc, baseTreeSHA)
	if err != nil {
		return nil, err
	}
	for path, leaf := range leaves {
		if leaf.mode == executableMode {
			executable[path] = true
		}
	}
	return executable, nil
}

const executableMode = "100755"

func treeEntryMode(executable bool) string {
	if executable {
		return executableMode
	}
	return "100644"
}

type treeLeaf struct {
	mode string
	sha  string
}

// flatTree maps every non-directory path in a tree to its mode and object. Git trees can't contain
// empty directories, so two trees are identical exactly when their flat trees are equal.
func (p *Processor) flatTree(ctx context.Context, rc *repoChanges, treeSHA string) (map[string]treeLeaf, error) {
	tree, _, err := p.gh.Git.GetTree(ctx, rc.owner, rc.repo, treeSHA, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get tree %s: %w", treeSHA, err)
	}
	if tree.GetTruncated() {
		return nil, errTreeTruncated
	}
	leaves := make(map[string]treeLeaf, len(tree.Entries))
	for _, entry := range tree.Entries {
		if entry.GetType() != "tree" {
			leaves[entry.GetPath()] = treeLeaf{mode: entry.GetMode(), sha: entry.GetSHA()}
		}
	}
	return leaves, nil
}

// commitViaGit is the clone-and-push equivalent of commitViaAPI, reporting whether anything was
// pushed.
func (p *Processor) commitViaGit(rc *repoChanges, baseBranch string, targetBranch string) (bool, error) {
	dir, err := p.cloneRepo(rc.owner, rc.repo)
	if err != nil {
		return false, err
	}
	p.logger.Debugf("cloned %s to %s", rc.fullName(), dir)

	for _, c := range rc.changes {
		fullpath := path.Join(dir, c.dest)
		if err := os.MkdirAll(path.Dir(fullpath), 0777); err != nil {
			return false, fmt.Errorf("failed to create %s: %s", path.Dir(fullpath), err)
		}
		if err := os.WriteFile(fullpath, c.content, 0644); err != nil {
			return false, fmt.Errorf("failed to write file %s: %s", fullpath, err)
		}
		if err := p.runGit(dir, "add", "--", c.dest); err != nil {
			return false, fmt.Errorf("failed to add %s: %s", fullpath, err)
		}
	}

	if err := p.runGit(dir, "commit", "-m", rc.commitMessage()); err != nil {
		return false, fmt.Errorf("failed to commit: %s", err)
	}

	pushArgs := []string{"push"}
	if targetBranch != baseBranch {
		upToDate, err := p.remoteBranchMatchesHead(dir, targetBranch)
		if err != nil {
			return false, err
		}
		// Mirrors the API path: re-pushing an unchanged PR would needlessly re-trigger its CI.
		if upToDate {
			return false, nil
		}
		pushArgs = append(pushArgs, "--force")
	}
	pushArgs = append(pushArgs, "origin", "HEAD:refs/heads/"+targetBranch)
	if err := p.runGit(dir, pushArgs...); err != nil {
		return false, fmt.Errorf("failed to push: %s", err)
	}
	return true, nil
}

// remoteBranchMatchesHead reports whether branch on origin already has the same tree as HEAD.
func (p *Processor) remoteBranchMatchesHead(dir string, branch string) (bool, error) {
	ref := "refs/heads/" + branch
	if err := p.runGit(dir, "ls-remote", "--exit-code", "origin", ref); err != nil {
		// ls-remote --exit-code uses 2 specifically for "no matching ref".
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return false, nil
		}
		return false, fmt.Errorf("failed to look up branch %s: %s", branch, err)
	}
	if err := p.runGit(dir, "fetch", "--depth=1", "origin", ref); err != nil {
		return false, fmt.Errorf("failed to fetch branch %s: %s", branch, err)
	}
	remoteTree, err := p.gitOutput(dir, "rev-parse", "FETCH_HEAD^{tree}")
	if err != nil {
		return false, fmt.Errorf("failed to read tree of branch %s: %s", branch, err)
	}
	localTree, err := p.gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return false, fmt.Errorf("failed to read tree of HEAD: %s", err)
	}
	return remoteTree == localTree, nil
}

func (p *Processor) cloneRepo(owner string, repo string) (string, error) {
	dir := path.Join(os.TempDir(), "FileMaintainer", "clones", owner, repo)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("failed to remove %s: %s", dir, err)
	}
	if err := os.MkdirAll(path.Dir(dir), 0777); err != nil {
		return "", fmt.Errorf("failed to create %s: %s", path.Dir(dir), err)
	}
	ref := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	if err := p.runGit("", "clone", "--depth=1", "--", ref, dir); err != nil {
		return "", fmt.Errorf("failed to clone %s/%s: %s", owner, repo, err)
	}
	return dir, nil
}

func (p *Processor) runGit(dir string, args ...string) error {
	_, err := p.gitOutput(dir, args...)
	return err
}

func (p *Processor) gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		p.logger.Debugf("%s: %s", cmd.String(), out)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.logger.Debugf("stderr: %s", exitErr.Stderr)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// findPullRequest returns the open FileMaintainer pull request into baseBranch, or nil if there is none.
func (p *Processor) findPullRequest(ctx context.Context, rc *repoChanges, baseBranch string) (*github.PullRequest, error) {
	existing, _, err := p.gh.PullRequests.List(ctx, rc.owner, rc.repo, &github.PullRequestListOptions{
		State: "open",
		Head:  rc.owner + ":" + pullRequestBranch,
		Base:  baseBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pull requests: %s", err)
	}
	if len(existing) == 0 {
		return nil, nil
	}
	return existing[0], nil
}

func (p *Processor) ensurePullRequest(ctx context.Context, rc *repoChanges, baseBranch string) error {
	existing, err := p.findPullRequest(ctx, rc, baseBranch)
	if err != nil {
		return err
	}
	if existing != nil {
		p.logger.Infof("pull request %s is open", existing.GetHTMLURL())
		return nil
	}

	pr, _, err := p.gh.PullRequests.Create(ctx, rc.owner, rc.repo, &github.NewPullRequest{
		Title: github.String(changeTitle),
		Head:  github.String(pullRequestBranch),
		Base:  github.String(baseBranch),
		Body:  github.String(rc.body()),
	})
	if err != nil {
		return fmt.Errorf("failed to open pull request: %s", err)
	}
	p.logger.Infof("opened pull request %s", pr.GetHTMLURL())
	return nil
}

func (p *Processor) applyToAllRepos(remote RemoteSpec, remoteName string, f func(owner string, repo string) error) error {
	resolved, err := p.resolver.ResolveRemote(remote, remoteName)
	p.logger.Debugf("resolved %s as %+v %s", remote, resolved, err)
	if err != nil {
		return err
	}

	for _, repo := range resolved.Repos {
		if len(p.onlyRepo) == 0 || strings.EqualFold(repo, p.onlyRepo) {
			err := f(resolved.Owner, repo)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
