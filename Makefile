.PHONY: build run clean fmt tidy

BUILD_DIR=build
BINARY_NAME=gateway.exe
BINARY_PATH=$(BUILD_DIR)/$(BINARY_NAME)

build:
	if not exist $(BUILD_DIR) mkdir $(BUILD_DIR)
	go build -o $(BINARY_PATH) cmd/gateway/main.go

run: build
	./$(BINARY_PATH)

clean:
	if exist $(BUILD_DIR) rmdir /s /q $(BUILD_DIR)
	if exist gateway.db del gateway.db

fmt:
	go fmt ./...

tidy:
	go mod tidy
