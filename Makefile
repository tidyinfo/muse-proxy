.PHONY: build test vet fmt clean

build:
	go build -o bin/ ./...

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l .
	@test -z "$$(gofmt -l .)" || (echo "gofmt: files need formatting"; exit 1)

clean:
	rm -rf bin/
