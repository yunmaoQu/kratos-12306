SHELL := /bin/bash

PROTOC ?= protoc
API_DIR := api
THIRD := third_party/googleapis
PROTO_IMPORTS := -I . -I $(API_DIR) -I $(THIRD)

PROTO_FILES := $(shell find $(API_DIR) -name "*.proto")

.PHONY: deps
deps:
	@if [ ! -d "$(THIRD)" ]; then \
	  git clone --depth=1 https://github.com/googleapis/googleapis $(THIRD); \
	fi
	@echo "Install protoc plugins if missing:"
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@go install github.com/go-kratos/kratos/cmd/protoc-gen-go-http/v2@latest

.PHONY: proto
proto: deps
	@for f in $(PROTO_FILES); do \
	  echo ">> protoc $$f"; \
	  $(PROTOC) $(PROTO_IMPORTS) \
	    --go_out=paths=source_relative:. \
	    --go-grpc_out=paths=source_relative:. \
	    --go-http_out=paths=source_relative:. \
	    $$f; \
	done

.PHONY: build
build:
	@go build -o bin/seat ./app/seat/cmd/seat
	@go build -o bin/order ./app/order/cmd/order
	@go build -o bin/payment ./app/payment/cmd/payment

.PHONY: run-seat run-order run-payment
run-seat:
	@go run ./app/seat/cmd/seat
run-order:
	@ORDER_GRPC_ADDR=:9001 SEAT_GRPC_ADDR=127.0.0.1:9000 go run ./app/order/cmd/order
run-payment:
	@ORDER_GRPC_ADDR=127.0.0.1:9001 go run ./app/payment/cmd/payment

.PHONY: docker
docker:
	docker build -t crud/seat:dev ./app/seat
	docker build -t crud/order:dev ./app/order
	docker build -t crud/payment:dev ./app/payment