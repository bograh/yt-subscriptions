APP      := yt-subscriptions
BIN      := bin/$(APP)
IMAGE    ?= $(APP):latest
PORT     ?= 8080
TEMPL    := go tool templ

.DEFAULT_GOAL := help
.PHONY: help generate build run dev fmt vet tidy check clean docker-build docker-run

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

generate: ## Generate Go code from .templ files
	$(TEMPL) generate

build: generate ## Build the binary into bin/
	go build -o $(BIN) .

run: build ## Build and run the dashboard
	PORT=$(PORT) ./$(BIN)

dev: ## Run with live reload (templ watch + browser proxy on :7331)
	$(TEMPL) generate --watch --proxy=http://localhost:$(PORT) --cmd="go run ."

fmt: ## Format Go and templ files
	go fmt ./...
	$(TEMPL) fmt views

vet: generate ## Run go vet
	go vet ./...

tidy: ## Tidy go.mod
	go mod tidy

check: fmt vet build ## Format, vet and build

clean: ## Remove build output and generated templ code
	rm -rf bin
	find . -name '*_templ.go' -delete

docker-build: ## Build the Docker image
	docker build -t $(IMAGE) .

docker-run: ## Run the image with .env and a persistent data volume
	docker run --rm -it -p $(PORT):8080 --env-file .env \
		-e OAUTH_REDIRECT_URL=http://localhost:$(PORT)/auth/callback \
		-v $(APP)-data:/data $(IMAGE)
