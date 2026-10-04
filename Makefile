.PHONY: lint test test-int build

lint:
	gofmt -l . | grep . && exit 1 || true
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...

test:
	go test -race -count=1 ./...

test-int:
	go test -race -count=1 -tags=integration ./internal/store/postgres/...

build:
	go build -o bin/recon-cli ./cmd/recon-cli
	go build -o bin/recon-server ./cmd/recon-server
