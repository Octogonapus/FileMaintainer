# FileMaintainer

Maintains files in your repositories so you don't have to.

Supports GitHub.
Supports individual repositories, some/all repositories of a user(s), and some/all repositories of an organization(s).
GitHub Enterprise is not directly supported, just list your organizations instead.

Changes are applied to the default branch.
Updates are primarily made via the GitHub API; if this fails, Git will be used instead.

Requires a GitHub token in the `GITHUB_TOKEN` environment variable with scopes: `repo`, `workflow`.

## Install

### Binary Installation

Download an appropriate binary from the [latest release](https://github.com/Octogonapus/FileMaintainer/releases/latest).

### Manual Installation

```sh
git clone https://github.com/Octogonapus/FileMaintainer
cd FileMaintainer
go build
go install
```

## Usage

Create a `FileMaintainer.toml` file, along with supporting files, like the example below.
Then run `FileMaintainer` in the same directory as `FileMaintainer.toml`.
Review the changes and apply them: `FileMaintainer --dry-run=false`.

### Example FileMaintainer.toml

```toml
[remote.entire_org]
owner = "MyOrg" # owner signifies an organization
exclude_repos = ["SomeRepo"] # filter out some repositories by name

[remote.some_user]
user = "SomeUsername" # user signifies an individual user

[remote.julia_pkgs]
owner = "MyOrg"
repo_glob = "*.jl" # filter by repository name

[remote.single_repo]
owner = "MyOrg"
repo = "MyRepo" # select a single repository by name

[file.gitleaks]
path = "gitleaks/gitleaks.yml" # a local file in the same directory as this file
dest = ".github/workflows/gitleaks.yml" # the remote file path relative to the repository root
remotes = ["entire_org"]

[file.juliafmt]
path = "juliafmt/action.yml" # a local file in the same directory as this file
dest = ".github/workflows/formatter.yml" # the remote file path relative to the repository root
remotes = ["julia_pkgs"]
```

### Example GitHub Actions Workflow

```yml
name: FileMaintainer

on:
  workflow_dispatch:

jobs:
  FileMaintainer:
    runs-on: ubuntu-latest

    env:
      VERSION: 0.1.0

    steps:
      - uses: actions/checkout@v3

      - name: Install FileMaintainer
        run: |
          wget -nv "https://github.com/Octogonapus/FileMaintainer/releases/download/v$VERSION/FileMaintainer_${VERSION}_linux_amd64.tar.gz"
          tar -xf "FileMaintainer_${VERSION}_linux_amd64.tar.gz"

      - name: Run FileMaintainer
        env:
          GITHUB_TOKEN: ${{ secrets.SOME_PAT }}
        run: ./FileMaintainer --dry-run=true --debug # FIXME turn off dry runs after you have tested this
```
