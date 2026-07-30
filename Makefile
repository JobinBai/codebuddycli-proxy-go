APP := codebuddycli-proxy
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet cross docker
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/$(APP)
test:
	go test ./...
vet:
	go vet ./...
cross:
	./scripts/cross-build.sh $(VERSION)
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(APP):$(VERSION) .
