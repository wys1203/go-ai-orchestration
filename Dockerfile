# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gao ./cmd/gao

# github-mcp-server is a single static binary; borrow it from the official image.
FROM ghcr.io/github/github-mcp-server:latest AS ghmcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gao /usr/local/bin/gao
COPY --from=ghmcp /server/github-mcp-server /usr/local/bin/github-mcp-server
COPY skills /app/skills
WORKDIR /app
ENV GAO_CONFIG=/app/config.yaml GAO_LOG_FORMAT=json
ENTRYPOINT ["gao"]
CMD ["run"]
