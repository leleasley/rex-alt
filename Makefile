BINARY := rex-alt
GO ?= go

.PHONY: build test vet fmt docker clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/rex-alt

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

docker:
	docker build -t rex-alt:local .

clean:
	rm -f $(BINARY)
