.PHONY: build run clean fmt tidy

BINARY_NAME=gateway.exe

build:
	go build -o $(BINARY_NAME) cmd/gateway/main.go

run: build
	./$(BINARY_NAME)

clean:
	if exist $(BINARY_NAME) del $(BINARY_NAME)
	if exist gateway.db del gateway.db

fmt:
	go fmt ./...

tidy:
	go mod tidy
