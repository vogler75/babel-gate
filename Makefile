.PHONY: all build test run clean

all: build

build:
	@mkdir -p bin
	go build -o bin/babelgate ./cmd/router
	@ln -sf babelgate bin/llm-router 2>/dev/null || true

test:
	go test -v ./...

run: build
	./bin/babelgate -config config.example.yaml

clean:
	rm -rf bin
