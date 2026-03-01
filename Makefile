VERSION ?= $(shell git describe --tags --always)

.PHONY: build
build:
	go build -ldflags "-X main.version=$(VERSION:v%=%)" -o maestro ./cmd/maestro/
