# deps-merger - merge safe Dependabot PRs

binary_name := "deps-merger"
install_dir := env("HOME") / ".local/bin"

version := env("VERSION", "dev")
git_commit := `git rev-parse --short HEAD 2>/dev/null || echo "unknown"`
build_time := `date -u '+%Y-%m-%dT%H:%M:%SZ'`
ldflags := "-ldflags \"-X github.com/sglavoie/deps-merger/cmd.Version=" + version + " -X github.com/sglavoie/deps-merger/cmd.GitCommit=" + git_commit + " -X github.com/sglavoie/deps-merger/cmd.BuildTime=" + build_time + "\""

# Show available recipes
help:
    @just --list

# Build the binary in the current directory
build:
    go build {{ ldflags }} -o {{ binary_name }} .

# Install the binary to ~/.local/bin
install: build
    @mkdir -p {{ install_dir }}
    rm -f {{ install_dir }}/{{ binary_name }}
    cp {{ binary_name }} {{ install_dir }}/{{ binary_name }}

# Bump patch, minor, or major and trigger the GitHub release workflow
release bump:
    #!/usr/bin/env bash
    set -euo pipefail
    bump={{ quote(bump) }}
    case "$bump" in
        patch|minor|major) ;;
        *) echo "Usage: just release patch|minor|major" >&2; exit 1 ;;
    esac
    if [[ "$(git branch --show-current)" != main ]]; then
        echo "Release from the main branch." >&2
        exit 1
    fi
    if [[ -n "$(git status --porcelain)" ]]; then
        echo "Commit or stash changes before releasing." >&2
        exit 1
    fi
    git fetch --tags origin main
    if ! git merge-base --is-ancestor FETCH_HEAD HEAD; then
        echo "Update main from origin before releasing." >&2
        exit 1
    fi
    latest=$(git tag --sort=-version:refname | sed -nE '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/p' | sed -n '1p')
    latest=${latest:-v0.0.0}
    IFS=. read -r major minor patch <<< "${latest#v}"
    case "$bump" in
        patch) patch=$((patch + 1)) ;;
        minor) minor=$((minor + 1)); patch=0 ;;
        major) major=$((major + 1)); minor=0; patch=0 ;;
    esac
    tag="v$major.$minor.$patch"
    echo "Releasing $latest -> $tag"
    git tag -a "$tag" -m "Release $tag"
    if ! git push --atomic origin HEAD:refs/heads/main "refs/tags/$tag"; then
        echo "Push failed; local tag $tag was kept. Retry with:" >&2
        echo "  git push --atomic origin HEAD:refs/heads/main refs/tags/$tag" >&2
        exit 1
    fi
    echo "Pushed $tag. GitHub Actions will run CI and publish the release:"
    echo "https://github.com/sglavoie/deps-merger/actions/workflows/release.yml"

# Run tests
test:
    go test -race ./...

# Test coverage per function (writes coverage.out)
cover:
    go test -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -n 1

# Format code
fmt:
    gofmt -w .

# Vet code
vet:
    go vet ./...

# Tidy modules
tidy:
    go mod tidy

# Static analysis (staticcheck is pinned as a tool in go.mod)
lint:
    go tool staticcheck ./...

# Known vulnerabilities in reachable code (govulncheck is pinned in go.mod)
vuln:
    go tool govulncheck ./...

# Remove build, coverage, demo and release output
clean:
    rm -rf {{ binary_name }} coverage.out .demo dist completions

# Everything CI should check
ci: vet lint vuln test
    go vet -tags demo ./...
    go tool staticcheck -tags demo ./...
    go tool govulncheck -tags demo ./...
    go test -race -tags demo ./...
    GOOS=windows go vet ./...
    GOOS=windows go vet -tags demo ./...
    @test -z "$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
    go mod tidy -diff

# Record docs/demo.gif with VHS against fake repositories (nothing real is touched)
demo:
    go build -tags demo -o .demo/bin/{{ binary_name }} .
    vhs demo/demo.tape

# Try the CLI against fake repositories, e.g. `just try run --review --apply`
try *args:
    go run -tags demo . {{ args }}

# Dry run against the configured repos
dry-run *args:
    go run . run {{ args }}

# Run the GitHub App locally with the API model, Claude Code, or Codex
app-local agent="":
    ./scripts/run-local.sh {{ quote(agent) }}
