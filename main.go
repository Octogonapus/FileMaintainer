package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/gofri/go-github-ratelimit/github_ratelimit"
	"github.com/google/go-github/v52/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

type Config struct {
	Remote map[string]RemoteSpec
	File   map[string]FileSpec
}

type RemoteSpec struct {
	Org          string
	User         string
	Repo         string
	RepoGlob     string   `toml:"repo_glob"`
	ExcludeRepos []string `toml:"exclude_repos"`
}

func (rs *RemoteSpec) Owner() string {
	if len(rs.Org) > 0 {
		return rs.Org
	} else {
		return rs.User
	}
}

type FileSpec struct {
	Path    string
	Dest    string
	Remotes []string
}

func main() {
	configFilePath := flag.String("config", "FileMaintainer.toml", "Path to FileMaintainer.toml file.")
	dryRun := flag.Bool("dry-run", true, "")
	debug := flag.Bool("debug", false, "")
	onlyRepo := flag.String("only-repo", "", "Update this repository only")
	flag.Parse()

	logger, err := NewLogger(*debug)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()
	sugar := logger.Sugar()

	var config Config
	_, err = toml.DecodeFile(*configFilePath, &config)
	if err != nil {
		panic(err)
	}

	err = validateConfig(config)
	if err != nil {
		panic(err)
	}

	gh := NewGH()

	processor := NewProcessor(*dryRun, *onlyRepo, gh, sugar)
	err = processor.ProcessFiles(config)
	if err != nil {
		panic(err)
	}
}

func NewLogger(debug bool) (*zap.Logger, error) {
	level := zap.InfoLevel
	if debug {
		level = zap.DebugLevel
	}
	config := zap.Config{
		Level:            zap.NewAtomicLevelAt(level),
		Development:      true,
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}
	return config.Build()
}

func NewGH() *github.Client {
	token, hasToken := os.LookupEnv("GITHUB_TOKEN")
	if !hasToken {
		panic("Must have a GITHUB_TOKEN environment variable.")
	}
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	rateLimiter, err := github_ratelimit.NewRateLimitWaiterClient(tc.Transport)
	if err != nil {
		panic(err)
	}
	gh := github.NewClient(rateLimiter)
	return gh
}

func validateConfig(config Config) error {
	err := validateRemotes(config.Remote)
	if err != nil {
		return err
	}

	err = validateFiles(config)
	return err
}

// Valid remotes:
//   - Have either an org or a user
func validateRemotes(remoteSpec map[string]RemoteSpec) error {
	for name, remote := range remoteSpec {
		if (len(remote.Org) == 0) == (len(remote.User) == 0) {
			return fmt.Errorf("remote %s must have either an org or a user", name)
		}
	}
	return nil
}

// Valid files:
//   - Point to real files
//   - Have a destination path
//   - Reference valid remotes
func validateFiles(config Config) error {
	for name, file := range config.File {
		// Points to a real file
		if !isFile(file.Path) {
			return fmt.Errorf("file %s must have a path which exists and is a file", name)
		}

		// Has a destination path
		if len(file.Dest) == 0 {
			return fmt.Errorf("file %s must have a dest", name)
		}

		// The remote in the file must be in the remotes
		for _, remoteInFile := range file.Remotes {
			found := false
			for remote := range config.Remote {
				if remote == remoteInFile {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("file %s specifies a remote %s which is not part of the remotes table", name, remoteInFile)
			}
		}
	}
	return nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	if err == nil {
		return !info.IsDir()
	} else {
		return false
	}
}
