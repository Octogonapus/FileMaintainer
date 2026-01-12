package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/google/go-github/v52/github"
	"go.uber.org/zap"
)

type Processor struct {
	dryRun        bool
	onlyRepo      string
	gh            *github.Client
	resolver      *RemoteResolver
	remoteSpecMap map[string]RemoteSpec
	logger        *zap.SugaredLogger
}

func NewProcessor(dryRun bool, onlyRepo string, gh *github.Client, logger *zap.SugaredLogger) *Processor {
	return &Processor{
		dryRun:        dryRun,
		onlyRepo:      onlyRepo,
		gh:            gh,
		logger:        logger,
		remoteSpecMap: make(map[string]RemoteSpec),
		resolver:      NewRemoteResolver(gh, logger),
	}
}

func (p *Processor) updateRemoteSpecMap(config Config) {
	for name, remote := range config.Remote {
		if _, ok := p.remoteSpecMap[name]; !ok {
			p.remoteSpecMap[name] = remote
		}
	}
}

func (p *Processor) ProcessFiles(config Config) error {
	p.updateRemoteSpecMap(config)
	for _, file := range config.File {
		err := p.ProcessFile(file, config)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) ProcessFile(file FileSpec, config Config) error {
	msg := fmt.Sprintf("Processing file %s", file.Dest)
	sep := strings.Repeat("-", len(msg))
	p.logger.Infof(sep)
	p.logger.Infof(msg)
	p.logger.Infof(sep)

	for _, remoteName := range file.Remotes {
		remote, ok := config.Remote[remoteName]
		if !ok {
			return fmt.Errorf("did not find a remote named %s in remotes", remoteName)
		}

		content, err := os.ReadFile(file.Path)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %s", file.Path, err)
		}

		err = p.applyToAllRepos(remote, remoteName, func(owner string, repo string) error {
			remoteContentResp, _, resp, err := p.gh.Repositories.GetContents(context.Background(), owner, repo, file.Dest, &github.RepositoryContentGetOptions{})
			if resp.StatusCode == 200 {
				// Avoid an update if the remote content doesn't need to change
				remoteContent, err := remoteContentResp.GetContent()
				if err == nil && remoteContent == string(content) {
					p.logger.Debugf("skipping %s/%s/%s because it does not need to be updated", owner, repo, file.Dest)
					return nil
				}

				if p.dryRun {
					p.printUpdateFileDryRun(remoteContent, string(content), owner, repo, file.Dest)
					return nil
				} else {
					err = p.updateFile(owner, repo, file.Dest, content, *remoteContentResp.SHA)
					if err != nil {
						return fmt.Errorf("failed to update file %s/%s/%s: %s", owner, repo, file.Dest, err)
					}
				}
			} else if resp.StatusCode == 404 {
				if p.dryRun {
					p.logger.Infof("would create file %s/%s/%s", owner, repo, file.Dest)
					return nil
				} else {
					err = p.createFile(owner, repo, file.Dest, content)
					if err != nil {
						return fmt.Errorf("failed to create file %s/%s/%s: %s", owner, repo, file.Dest, err)
					}
				}
			} else {
				return fmt.Errorf("failed to fetch contents of file %s/%s/%s: %s", owner, repo, file.Dest, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) printUpdateFileDryRun(remoteContent string, content string, owner string, repo string, dest string) {
	remoteContentLines := strings.Split(remoteContent, "\n")
	contentLines := strings.Split(content, "\n")
	linesOfDiff := 0
	for i, line := range contentLines {
		if i >= len(remoteContentLines) {
			linesOfDiff += len(remoteContentLines) - i
			break
		} else {
			if line != remoteContentLines[i] {
				linesOfDiff++
			}
		}
	}
	p.logger.Infof("would update %d lines in file %s/%s/%s", linesOfDiff, owner, repo, dest)
}

func (p *Processor) updateFile(owner string, repo string, dest string, content []byte, sha string) error {
	msg := fmt.Sprintf("FileMaintainer: Update %s", dest)
	_, resp, err := p.gh.Repositories.CreateFile(context.Background(),
		owner,
		repo,
		dest,
		&github.RepositoryContentFileOptions{
			Message: &msg,
			Content: content,
			SHA:     &sha,
		})
	if resp.StatusCode == 409 {
		p.logger.Debugf("could not update file via API due to conflict (will try git-based update): %s", err)
		return p.updateFileViaGit(owner, repo, dest, content)
	} else {
		if err != nil {
			return err
		}
		p.logger.Infof("updated %s/%s/%s", owner, repo, dest)
		return nil
	}
}

func (p *Processor) createFile(owner string, repo string, dest string, content []byte) error {
	msg := fmt.Sprintf("FileMaintainer: Create %s", dest)
	_, resp, err := p.gh.Repositories.CreateFile(context.Background(),
		owner,
		repo,
		dest,
		&github.RepositoryContentFileOptions{
			Message: &msg,
			Content: content,
		})
	if resp.StatusCode == 409 {
		p.logger.Debugf("could not create file via API due to conflict (will try git-based update): %s", err)
		return p.updateFileViaGit(owner, repo, dest, content)
	} else {
		if err != nil {
			return err
		}
		p.logger.Infof("created %s/%s/%s", owner, repo, dest)
		return nil
	}
}

func (p *Processor) updateFileViaGit(owner string, repo string, dest string, content []byte) error {
	if p.dryRun {
		p.logger.Infof("would update file %s/%s/%s", owner, repo, dest)
		return nil
	}

	dir, err := p.cloneRepo(owner, repo)
	if err != nil {
		return err
	}
	p.logger.Debugf("cloned %s/%s to %s: %s", owner, repo, dir, err)

	err = p.writeFileToRepo(dir, dest, content)
	if err != nil {
		return err
	}

	p.logger.Debugf("pushing %s", dir)
	err = p.pushRepo(dir)
	p.logger.Debugf("pushed %s: %s", dir, err)
	return err
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
	cmd := exec.Command("git", "clone", "--depth=1", "--", ref, dir)
	if out, err := cmd.Output(); err != nil {
		p.logger.Debugf("%s: %s", cmd.String(), out)
		return "", fmt.Errorf("failed to clone %s/%s: %s", owner, repo, err)
	}
	return dir, nil
}

func (p *Processor) writeFileToRepo(dir string, dest string, content []byte) error {
	fullpath := path.Join(dir, dest)
	if err := os.WriteFile(fullpath, content, 0777); err != nil {
		return fmt.Errorf("failed to write file %s: %s", fullpath, err)
	}

	cmd := exec.Command("git", "add", "--", fullpath)
	cmd.Dir = dir
	if out, err := cmd.Output(); err != nil {
		p.logger.Debugf("%s: %s", cmd.String(), out)
		return fmt.Errorf("failed to add %s: %s", fullpath, err)
	}

	msg := fmt.Sprintf("FileMaintainer: Create or Update %s", dest)
	cmd = exec.Command("git", "commit", "-m", msg)
	cmd.Dir = dir
	if out, err := cmd.Output(); err != nil {
		p.logger.Debugf("%s: %s", cmd.String(), out)
		return fmt.Errorf("failed to commit %s: %s", fullpath, err)
	}

	return nil
}

func (p *Processor) pushRepo(dir string) error {
	cmd := exec.Command("git", "push")
	cmd.Dir = dir
	if out, err := cmd.Output(); err != nil {
		p.logger.Debugf("%s: %s", cmd.String(), out)
		return err
	}
	return nil
}

func (p *Processor) applyToAllRepos(remote RemoteSpec, remoteName string, f func(owner string, repo string) error) error {
	resolved, err := p.resolver.ResolveRemote(remote, remoteName)
	p.logger.Debugf("resolved %s as %+v %s", remote, resolved, err)
	if err != nil {
		return err
	}

	for _, repo := range resolved.Repos {
		if len(p.onlyRepo) == 0 || (len(p.onlyRepo) > 0 && repo == p.onlyRepo) {
			err := f(resolved.Owner, repo)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
