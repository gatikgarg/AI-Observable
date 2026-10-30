.PHONY: build test verify
build:
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=dev" -o llmtap ./cmd/llmtap
test:
	go vet ./...
	go test -race ./...
verify: test
	go run ./cmd/verifydash
