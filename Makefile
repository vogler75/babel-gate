.PHONY: all build test run clean

all: build

build:
	@mkdir -p bin
	go build -o bin/babelgate ./cmd/router

test:
	go test -v ./...

run: build
	./bin/babelgate -config config.example.yaml

clean:
	rm -rf bin
