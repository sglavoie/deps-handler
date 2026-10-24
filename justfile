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
    go test -tags demo -run TestDemo ./cmd
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
