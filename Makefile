APP              := yt-subscriptions
BIN              := bin/$(APP)
IMAGE            ?= $(APP):latest
VERSION          ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PORT             ?= 8080
TEMPL            := go tool templ
TAILWIND_VERSION := v4.1.11
TAILWIND         := bin/tailwindcss

UNAME_S := $(shell uname -s | tr A-Z a-z)
UNAME_M := $(shell uname -m)
TW_OS   := $(if $(filter darwin,$(UNAME_S)),macos,linux)
TW_ARCH := $(if $(filter x86_64 amd64,$(UNAME_M)),x64,arm64)

.DEFAULT_GOAL := help
.PHONY: help generate css build run dev fmt vet test tidy check clean docker-build docker-run up down

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

$(TAILWIND):
	@mkdir -p bin
	curl -sSfL -o $@ https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(TW_OS)-$(TW_ARCH)
	chmod +x $@

generate: ## Generate Go code from .templ files
	$(TEMPL) generate

css: $(TAILWIND) ## Build static/app.css with the Tailwind CLI
	$(TAILWIND) -i styles/input.css -o static/app.css --minify

build: generate css ## Build the binary into bin/
	go build -ldflags="-X main.version=$(VERSION)" -o $(BIN) .

run: build ## Build and run the dashboard
	PORT=$(PORT) ./$(BIN)

dev: $(TAILWIND) ## Live reload: rebuilds CSS and templates (open the proxy on :7331)
	$(TAILWIND) -i styles/input.css -o static/app.css --watch=always & \
	$(TEMPL) generate --watch --proxy=http://localhost:$(PORT) --cmd="go run ."; \
	kill %1

fmt: ## Format Go and templ files
	go fmt ./...
	$(TEMPL) fmt views

vet: generate ## Run go vet
	go vet ./...

test: generate ## Run tests
	go test ./...

tidy: ## Tidy go.mod
	go mod tidy

check: fmt vet test build ## Format, vet, test and build

clean: ## Remove build output
	rm -rf bin

docker-build: ## Build the Docker image for this machine
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-run: ## Run the image with .env and a persistent data volume (localhost only)
	docker run --rm -it -p 127.0.0.1:$(PORT):8080 --env-file .env \
		-e PUBLIC_URL=http://localhost:$(PORT) -e ALLOW_NO_PASSWORD=1 \
		-v $(APP)-data:/data $(IMAGE)

up: ## Start with docker compose (set DASHBOARD_PASSWORD in .env)
	VERSION=$(VERSION) docker compose up -d --build

down: ## Stop the docker compose stack
	docker compose down
