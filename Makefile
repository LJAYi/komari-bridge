.PHONY: build build-linux-amd64 test verify clean

build:
	./scripts/build.sh

build-linux-amd64:
	GOOS=linux GOARCH=amd64 OUTPUT=bin/komari-bridge-linux-amd64 ./scripts/build.sh

test:
	go test ./...

verify:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...

clean:
	rm -rf bin
