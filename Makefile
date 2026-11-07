.PHONY: check

check:
	@unformatted=$$(gofmt -l go/); \
	if [ -n "$$unformatted" ]; then \
		echo "unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	cd go && go vet ./...
	cd go && go test -race ./...
	cd go && go build -o /dev/null ./cmd/herdsman ./cmd/herdsman-daemon
