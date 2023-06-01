package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"

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
	p.logger.Debugf("processing file %s", file.Dest)
	for _, remoteName := range file.Remotes {
		remote, ok := config.Remote[remoteName]
		if !ok {
			return fmt.Errorf("did not find a remote named %s in remotes", remoteName)
		}

		content, err := os.ReadFile(file.Path)
		if err != nil {
			return err
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

				err = p.updateFile(owner, repo, file.Dest, content, *remoteContentResp.SHA)
				if err != nil {
					return err
				}
			} else if resp.StatusCode == 404 {
				err = p.createFile(owner, repo, file.Dest, content)
				if err != nil {
					return err
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

func (p *Processor) updateFile(owner string, repo string, dest string, content []byte, sha string) error {
	if p.dryRun {
		p.logger.Infof("would create or update file %s/%s/%s", owner, repo, dest)
		return nil
	}

	msg := fmt.Sprintf("FileMaintainer: Create or Update %s", dest)
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
	if p.dryRun {
		p.logger.Infof("would create or update file %s/%s/%s", owner, repo, dest)
		return nil
	}

	msg := fmt.Sprintf("FileMaintainer: Create or Update %s", dest)
	_, resp, err := p.gh.Repositories.CreateFile(context.Background(),
		owner,
		repo,
		dest,
		&github.RepositoryContentFileOptions{
			Message: &msg,
			Content: content,
		})
	if resp.StatusCode == 409 {
		p.logger.Debugf("could not update file via API due to conflict (will try git-based update): %s", err)
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
		p.logger.Infof("would create or update file %s/%s/%s", owner, repo, dest)
		return nil
	}

	dir, err := p.cloneRepo(owner, repo)
	p.logger.Debugf("cloned %s/%s to %s: %s", owner, repo, dir, err)
	if err != nil {
		return err
	}

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
		return "", err
	}
	if err := os.MkdirAll(path.Dir(dir), 0777); err != nil {
		return "", err
	}
	ref := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	cmd := exec.Command("git", "clone", "--depth=1", "--", ref, dir)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return dir, nil
}

func (p *Processor) writeFileToRepo(dir string, dest string, content []byte) error {
	fullpath := path.Join(dir, dest)
	if err := os.WriteFile(fullpath, content, 0777); err != nil {
		return err
	}

	cmd := exec.Command("git", "add", "--", fullpath)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return err
	}

	msg := fmt.Sprintf("FileMaintainer: Create or Update %s", dest)
	cmd = exec.Command("git", "commit", "-m", msg)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

func (p *Processor) pushRepo(dir string) error {
	cmd := exec.Command("git", "push")
	cmd.Dir = dir
	return cmd.Run()
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
