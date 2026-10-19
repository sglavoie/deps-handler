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

# Everything CI should check
ci: vet lint test
    @test -z "$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

# Dry run against the configured repos
dry-run *args:
    go run . run {{ args }}
