BIN     := gao
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet lint run docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BIN) ./cmd/gao

test:
	go test -race ./...

vet:
	go vet ./...

run: build
	./bin/$(BIN) run -config config.yaml

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(BIN):$(VERSION) .

clean:
	rm -rf bin
