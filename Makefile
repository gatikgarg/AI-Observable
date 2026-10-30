.PHONY: build test verify run down logs
build:
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=dev" -o llmtap ./cmd/llmtap
test:
	go vet ./...
	go test -race ./...
verify: test
	go run ./cmd/verifydash
run:
	docker compose up --build -d
down:
	docker compose down
logs:
	docker compose logs -f
