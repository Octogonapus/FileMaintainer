package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/google/go-github/v52/github"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestProcessFilesContinuesPastFailingRepo(t *testing.T) {
	// bad fails on only one of its two files, so it must be skipped entirely rather than getting a
	// commit that leaves that file out.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/bad/contents/b.yml":
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		case "/repos/o/good":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case "/repos/o/good/git/ref/heads/main":
			fmt.Fprint(w, `{"object":{"sha":"base"}}`)
		case "/repos/o/good/git/commits/base":
			fmt.Fprint(w, `{"tree":{"sha":"basetree"}}`)
		case "/repos/o/good/pulls":
			fmt.Fprint(w, `[]`)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	gh := github.NewClient(nil)
	gh.BaseURL, _ = url.Parse(server.URL + "/")

	dir := t.TempDir()
	for _, name := range []string{"a.yml", "b.yml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{
		Remote: map[string]RemoteSpec{"r": {Org: "o", Repos: []string{"bad", "good"}}},
		File: map[string]FileSpec{
			"a": {Path: filepath.Join(dir, "a.yml"), Dest: "a.yml", Remotes: []string{"r"}},
			"b": {Path: filepath.Join(dir, "b.yml"), Dest: "b.yml", Remotes: []string{"r"}},
		},
	}

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core).Sugar()
	err := NewProcessor(true, "", gh, logger).ProcessFiles(config)

	if err == nil || !strings.Contains(err.Error(), "o/bad/b.yml") {
		t.Errorf("expected an error naming the failing file, got %v", err)
	}
	for _, want := range []string{"would create file o/good/a.yml", "would create file o/good/b.yml"} {
		if logs.FilterMessage(want).Len() != 1 {
			t.Errorf("expected log %q", want)
		}
	}
	for _, entry := range logs.FilterMessageSnippet("o/bad").All() {
		if strings.HasPrefix(entry.Message, "would") {
			t.Errorf("expected no changes to be applied to the failing repo, got %q", entry.Message)
		}
	}
	for _, want := range []string{
		"would push 2 file(s) to branch " + pullRequestBranch + " of o/good",
		"would open a pull request from " + pullRequestBranch + " to main in o/good",
	} {
		if logs.FilterMessage(want).Len() != 1 {
			t.Errorf("expected log %q", want)
		}
	}
}

func TestDryRunReportsWhetherRealRunWouldPush(t *testing.T) {
	const newContent = "new"
	newSHA := gitBlobSHA([]byte(newContent))
	// The base tree has run.sh as executable, so the real run would keep it executable; a PR
	// branch with the right content but the wrong mode is still out of date.
	baseTree := `{"tree":[
		{"path":"run.sh","mode":"100755","type":"blob","sha":"old"},
		{"path":"dir","mode":"040000","type":"tree","sha":"d1"},
		{"path":"dir/keep.txt","mode":"100644","type":"blob","sha":"keep"}]}`
	cases := []struct {
		name       string
		branchTree string
		want       string
	}{
		{"missing branch", "", "would push 1 file(s) to branch " + pullRequestBranch + " of o/r"},
		{"up to date", `{"tree":[
			{"path":"run.sh","mode":"100755","type":"blob","sha":"` + newSHA + `"},
			{"path":"dir","mode":"040000","type":"tree","sha":"d2"},
			{"path":"dir/keep.txt","mode":"100644","type":"blob","sha":"keep"}]}`,
			"branch " + pullRequestBranch + " of o/r is already up to date; would not push"},
		{"wrong mode", `{"tree":[
			{"path":"run.sh","mode":"100644","type":"blob","sha":"` + newSHA + `"},
			{"path":"dir/keep.txt","mode":"100644","type":"blob","sha":"keep"}]}`,
			"would push 1 file(s) to branch " + pullRequestBranch + " of o/r"},
		{"base moved on", `{"tree":[
			{"path":"run.sh","mode":"100755","type":"blob","sha":"` + newSHA + `"},
			{"path":"dir/keep.txt","mode":"100644","type":"blob","sha":"outdated"}]}`,
			"would push 1 file(s) to branch " + pullRequestBranch + " of o/r"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("dry run made a write request %s %s", r.Method, r.URL.Path)
				}
				switch r.URL.Path {
				case "/repos/o/r":
					fmt.Fprint(w, `{"default_branch":"main"}`)
				case "/repos/o/r/git/ref/heads/main":
					fmt.Fprint(w, `{"object":{"sha":"base"}}`)
				case "/repos/o/r/git/commits/base":
					fmt.Fprint(w, `{"tree":{"sha":"basetree"}}`)
				case "/repos/o/r/git/trees/basetree":
					fmt.Fprint(w, baseTree)
				case "/repos/o/r/git/ref/heads/" + pullRequestBranch:
					if c.branchTree == "" {
						http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
						return
					}
					fmt.Fprint(w, `{"object":{"sha":"pr"}}`)
				case "/repos/o/r/git/commits/pr":
					fmt.Fprint(w, `{"tree":{"sha":"prtree"}}`)
				case "/repos/o/r/git/trees/prtree":
					fmt.Fprint(w, c.branchTree)
				case "/repos/o/r/pulls":
					fmt.Fprint(w, `[{"html_url":"https://example.com/pr/1"}]`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			gh := github.NewClient(nil)
			gh.BaseURL, _ = url.Parse(server.URL + "/")

			core, logs := observer.New(zap.InfoLevel)
			rc := &repoChanges{owner: "o", repo: "r", changes: []fileChange{{dest: "run.sh", content: []byte(newContent), exists: true}}}
			if err := NewProcessor(true, "", gh, zap.New(core).Sugar()).applyChanges(rc, RepoSettings{}); err != nil {
				t.Fatal(err)
			}
			if logs.FilterMessage(c.want).Len() != 1 {
				t.Errorf("expected log %q, got %v", c.want, logs.All())
			}
			if logs.FilterMessage("pull request https://example.com/pr/1 is already open").Len() != 1 {
				t.Error("expected the open pull request to be reported")
			}
		})
	}
}

func TestCommitViaAPISkipsUpToDateBranch(t *testing.T) {
	for _, existingTree := range []string{"newtree", "staletree"} {
		t.Run(existingTree, func(t *testing.T) {
			var forcedUpdate, createdCommit bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /repos/o/r/git/ref/heads/main":
					fmt.Fprint(w, `{"ref":"refs/heads/main","object":{"sha":"base"}}`)
				case "GET /repos/o/r/git/commits/base":
					fmt.Fprint(w, `{"sha":"base","tree":{"sha":"basetree"}}`)
				case "POST /repos/o/r/git/blobs":
					fmt.Fprint(w, `{"sha":"blob"}`)
				case "POST /repos/o/r/git/trees":
					fmt.Fprint(w, `{"sha":"newtree"}`)
				case "GET /repos/o/r/git/ref/heads/" + pullRequestBranch:
					fmt.Fprint(w, `{"ref":"refs/heads/`+pullRequestBranch+`","object":{"sha":"pr"}}`)
				case "GET /repos/o/r/git/commits/pr":
					fmt.Fprintf(w, `{"sha":"pr","tree":{"sha":%q}}`, existingTree)
				case "POST /repos/o/r/git/commits":
					createdCommit = true
					fmt.Fprint(w, `{"sha":"newcommit"}`)
				case "PATCH /repos/o/r/git/refs/heads/" + pullRequestBranch:
					var body struct{ Force bool }
					_ = json.NewDecoder(r.Body).Decode(&body)
					forcedUpdate = body.Force
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			gh := github.NewClient(nil)
			gh.BaseURL, _ = url.Parse(server.URL + "/")

			rc := &repoChanges{owner: "o", repo: "r", changes: []fileChange{{dest: "a.yml", content: []byte("a")}}}
			pushed, err := NewProcessor(false, "", gh, zap.NewNop().Sugar()).commitViaAPI(context.Background(), rc, "main", pullRequestBranch)
			if err != nil {
				t.Fatal(err)
			}

			stale := existingTree == "staletree"
			if pushed != stale || createdCommit != stale || forcedUpdate != stale {
				t.Errorf("pushed=%v createdCommit=%v forcedUpdate=%v, want all %v", pushed, createdCommit, forcedUpdate, stale)
			}
		})
	}
}

func TestRemoteBranchMatchesHead(t *testing.T) {
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "test")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %s", args, err, out)
		}
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	clone := filepath.Join(t.TempDir(), "clone")
	git("", "init", "--bare", origin)
	git("", "clone", origin, clone)
	commitFile := func(content string) {
		if err := os.WriteFile(filepath.Join(clone, "a.yml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		git(clone, "add", "a.yml")
		git(clone, "commit", "-m", content)
	}
	p := NewProcessor(false, "", nil, zap.NewNop().Sugar())
	check := func(want bool) {
		t.Helper()
		got, err := p.remoteBranchMatchesHead(clone, "b")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("remoteBranchMatchesHead = %v, want %v", got, want)
		}
	}

	commitFile("one")
	check(false) // branch does not exist yet
	git(clone, "push", "origin", "HEAD:refs/heads/b")
	check(true)
	// A different commit with the same tree still counts as up to date.
	git(clone, "commit", "--amend", "-m", "reworded")
	check(true)
	commitFile("two")
	check(false)
}

func TestGitBlobSHAMatchesGit(t *testing.T) {
	for _, content := range []string{"", "hello\n", "\xff\xfe binary"} {
		cmd := exec.Command("git", "hash-object", "--stdin")
		cmd.Stdin = strings.NewReader(content)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git hash-object failed: %s", err)
		}
		if got, want := gitBlobSHA([]byte(content)), strings.TrimSpace(string(out)); got != want {
			t.Errorf("gitBlobSHA(%q) = %s, want %s", content, got, want)
		}
	}
}

func TestCountChangedLines(t *testing.T) {
	cases := []struct {
		before, after string
		want          int
	}{
		{"a\nb", "a\nb", 0},
		{"a\nb", "a\nc", 1},
		{"a", "a\nb\nc", 2},
		{"a\nb\nc", "a", 2},
	}
	for _, c := range cases {
		if got := countChangedLines(c.before, c.after); got != c.want {
			t.Errorf("countChangedLines(%q, %q) = %d, want %d", c.before, c.after, got, c.want)
		}
	}
}

func TestShouldFallBackToGit(t *testing.T) {
	apiErr := func(status int) error {
		return fmt.Errorf("wrapped: %w", &github.ErrorResponse{Response: &http.Response{StatusCode: status}})
	}
	for _, status := range []int{409, 422} {
		if !shouldFallBackToGit(apiErr(status)) {
			t.Errorf("expected a %d to fall back to git", status)
		}
	}
	for _, status := range []int{401, 403, 404, 500} {
		if shouldFallBackToGit(apiErr(status)) {
			t.Errorf("expected a %d not to fall back to git", status)
		}
	}
	if !shouldFallBackToGit(fmt.Errorf("wrapped: %w", errTreeTruncated)) {
		t.Error("expected a truncated tree to fall back to git")
	}
	if shouldFallBackToGit(nil) || shouldFallBackToGit(errors.New("other")) {
		t.Error("expected nil and unrelated errors not to fall back to git")
	}
}

func TestReadmeExampleConfigDecodes(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(string(readme), "```toml\n")
	if !found {
		t.Fatal("README has no toml example")
	}
	example, _, _ := strings.Cut(rest, "```")

	var config Config
	md, err := toml.Decode(example, &config)
	if err != nil {
		t.Fatal(err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		t.Errorf("README example has unrecognized keys: %v", undecoded)
	}
	if err := validateRemotes(config.Remote); err != nil {
		t.Error(err)
	}
	if err := validateRepoSettings(config.Repo); err != nil {
		t.Error(err)
	}
}

func TestValidateRepoSettings(t *testing.T) {
	valid := map[string]RepoSettings{"MyOrg/MyRepo": {PushToDefaultBranch: true}}
	if err := validateRepoSettings(valid); err != nil {
		t.Errorf("expected %v to be valid: %s", valid, err)
	}

	for _, name := range []string{"MyRepo", "/MyRepo", "MyOrg/", "MyOrg/MyRepo/extra"} {
		if err := validateRepoSettings(map[string]RepoSettings{name: {}}); err == nil {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestRepoSettingsForIsCaseInsensitive(t *testing.T) {
	config := Config{Repo: map[string]RepoSettings{"myorg/myrepo": {PushToDefaultBranch: true}}}
	if !repoSettingsFor(config, "MyOrg/MyRepo").PushToDefaultBranch {
		t.Error("expected settings to match regardless of case")
	}
	if repoSettingsFor(config, "MyOrg/Other").PushToDefaultBranch {
		t.Error("expected unconfigured repos to default to opening a pull request")
	}
}

func TestCommitMessage(t *testing.T) {
	rc := &repoChanges{owner: "o", repo: "r", changes: []fileChange{{dest: "a.yml", exists: true}, {dest: "b.yml"}}}
	if got, want := rc.commitMessage(), "FileMaintainer Update\n\n- Update a.yml\n- Create b.yml"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
