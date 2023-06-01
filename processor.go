package main

import (
	"context"
	"fmt"
	"os"

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
			msg := fmt.Sprintf("FileMaintainer: Create or Update %s", file.Dest)
			remoteContentResp, _, resp, err := p.gh.Repositories.GetContents(context.Background(), owner, repo, file.Dest, &github.RepositoryContentGetOptions{})
			if resp.StatusCode == 200 {
				// Avoid an update if the remote content doesn't need to change
				remoteContent, err := remoteContentResp.GetContent()
				if err == nil && remoteContent == string(content) {
					p.logger.Debugf("skipping %s/%s/%s because it does not need to be updated", owner, repo, file.Dest)
					return nil
				}

				if p.dryRun {
					p.logger.Infof("would create or update file %s/%s/%s", owner, repo, file.Dest)
					return nil
				}

				_, _, err = p.gh.Repositories.CreateFile(context.Background(),
					owner,
					repo,
					file.Dest,
					&github.RepositoryContentFileOptions{
						Message: &msg,
						Content: content,
						SHA:     remoteContentResp.SHA,
					})
				if resp.StatusCode == 409 {
					p.logger.Errorf("could not update file due to conflict (will continue): %s", err)
				} else {
					if err != nil {
						return err
					}
					p.logger.Infof("updated %s/%s/%s", owner, repo, file.Dest)
				}
			} else if resp.StatusCode == 404 {
				if p.dryRun {
					p.logger.Infof("would create or update file %s/%s/%s", owner, repo, file.Dest)
					return nil
				}

				_, resp, err := p.gh.Repositories.CreateFile(context.Background(),
					owner,
					repo,
					file.Dest,
					&github.RepositoryContentFileOptions{
						Message: &msg,
						Content: content,
					})
				if resp.StatusCode == 409 {
					p.logger.Errorf("could not create file due to conflict (will continue): %s", err)
				} else {
					if err != nil {
						return err
					}
					p.logger.Infof("created %s/%s/%s", owner, repo, file.Dest)
				}
			} else if resp.StatusCode == 403 {
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
