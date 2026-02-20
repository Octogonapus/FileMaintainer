package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/go-github/v52/github"
	"go.uber.org/zap"
)

type ResolvedRemote struct {
	Owner string
	Repos []string
}

type RemoteResolver struct {
	resolved    map[string]*ResolvedRemote
	mainLock    *sync.Mutex
	remoteLocks map[string]*sync.Mutex
	gh          *github.Client
	logger      *zap.SugaredLogger
}

func NewRemoteResolver(gh *github.Client, logger *zap.SugaredLogger) *RemoteResolver {
	return &RemoteResolver{
		resolved:    make(map[string]*ResolvedRemote),
		mainLock:    &sync.Mutex{},
		remoteLocks: make(map[string]*sync.Mutex),
		gh:          gh,
		logger:      logger,
	}
}

func (resolver *RemoteResolver) ensureLockPresent(remoteName string) {
	resolver.mainLock.Lock()
	defer resolver.mainLock.Unlock()
	if _, ok := resolver.remoteLocks[remoteName]; !ok {
		resolver.remoteLocks[remoteName] = &sync.Mutex{}
	}
}

func (resolver *RemoteResolver) ResolveRemote(remote RemoteSpec, remoteName string) (*ResolvedRemote, error) {
	resolver.logger.Debugf("resolving remote %+v", remote)

	resolver.ensureLockPresent(remoteName)
	resolver.remoteLocks[remoteName].Lock()
	defer resolver.remoteLocks[remoteName].Unlock()

	if val, ok := resolver.resolved[remoteName]; ok {
		return val, nil
	}

	var resolved *ResolvedRemote

	if len(remote.Repo) > 0 {
		// The repo has been directly specified, nothing else to do
		resolved = &ResolvedRemote{Owner: remote.Owner(), Repos: []string{remote.Repo}}
	} else if len(remote.Repos) > 0 {
		resolved = &ResolvedRemote{Owner: remote.Owner(), Repos: remote.Repos}
	} else if len(remote.RepoGlob) > 0 {
		// Find the repos matching the glob
		repos, err := listAllRepos(resolver.gh, remote)
		if err != nil {
			return nil, err
		}

		repoNames := []string{}
		for _, repo := range repos {
			ok, _ := doublestar.Match(remote.RepoGlob, *repo.Name)
			if ok {
				repoNames = append(repoNames, *repo.Name)
			}
		}

		resolved = &ResolvedRemote{Owner: remote.Owner(), Repos: repoNames}
	} else {
		// Find all repos in the org
		repos, err := listAllRepos(resolver.gh, remote)
		if err != nil {
			return nil, err
		}

		repoNames := []string{}
		for _, repo := range repos {
			repoNames = append(repoNames, *repo.Name)
		}

		resolved = &ResolvedRemote{Owner: remote.Owner(), Repos: repoNames}
	}

	resolver.resolved[remoteName] = resolved
	return resolved, nil
}

func listAllRepos(gh *github.Client, remote RemoteSpec) ([]*github.Repository, error) {
	repos := []*github.Repository{}
	page := 1
	for {
		var (
			respRepos []*github.Repository
			resp      *github.Response
			err       error
		)
		if len(remote.User) > 0 {
			respRepos, resp, err = gh.Repositories.List(
				context.Background(),
				remote.User,
				&github.RepositoryListOptions{
					Affiliation: "owner",
					ListOptions: github.ListOptions{Page: page, PerPage: 100},
				},
			)
		} else {
			respRepos, resp, err = gh.Repositories.ListByOrg(
				context.Background(),
				remote.Org,
				&github.RepositoryListByOrgOptions{
					ListOptions: github.ListOptions{Page: page, PerPage: 100},
				},
			)
		}
		if resp.StatusCode != 200 {
			return []*github.Repository{}, fmt.Errorf("failed to list repos for %v: %s", remote, err)
		}

		// Don't try to update archived repos
		// Respect remote.ExcludeRepos
		for _, repo := range respRepos {
			excluded := false
			for _, excludedRepo := range remote.ExcludeRepos {
				if *repo.Name == excludedRepo {
					excluded = true
					break
				}
			}

			if !*repo.Archived && !excluded {
				repos = append(repos, repo)
			}
		}

		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return repos, nil
}
