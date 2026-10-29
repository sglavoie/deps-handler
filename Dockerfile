# Build the GitHub App and its deps-merger engine from this repository.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /server ./cmd/server

# Runs as root so it can write the volume Fly mounts at /data.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /server /server
ENV DMC_DB=/data/dmc.db
EXPOSE 8080
ENTRYPOINT ["/server"]
