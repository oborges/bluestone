.PHONY: all build test clean install install-service run docker-build docker-push k8s-deploy benchmark-suite help

# Variables
BINARY_NAME=bluestone
DOCKER_IMAGE=oborges/bluestone
VERSION?=1.0.0
GO=go
GOFLAGS=-v
LDFLAGS=-ldflags "-X main.Version=${VERSION}"

# Default target
all: clean build

# Build the binary
build:
	@echo "Building ${BINARY_NAME}..."
	@mkdir -p bin
	${GO} build ${GOFLAGS} ${LDFLAGS} -o bin/${BINARY_NAME} ./cmd/bluestone

# Build for multiple platforms
build-all:
	@echo "Building for multiple platforms..."
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 ${GO} build ${GOFLAGS} ${LDFLAGS} -o bin/${BINARY_NAME}-linux-amd64 ./cmd/bluestone
	GOOS=linux GOARCH=arm64 ${GO} build ${GOFLAGS} ${LDFLAGS} -o bin/${BINARY_NAME}-linux-arm64 ./cmd/bluestone
	GOOS=darwin GOARCH=amd64 ${GO} build ${GOFLAGS} ${LDFLAGS} -o bin/${BINARY_NAME}-darwin-amd64 ./cmd/bluestone
	GOOS=darwin GOARCH=arm64 ${GO} build ${GOFLAGS} ${LDFLAGS} -o bin/${BINARY_NAME}-darwin-arm64 ./cmd/bluestone

# Run tests
test:
	@echo "Running tests..."
	${GO} test -v -race -coverprofile=coverage.out ./...

# Run unit tests only
test-unit:
	@echo "Running unit tests..."
	${GO} test -v -race -short ./...

# Run integration tests
test-integration:
	@echo "Running integration tests..."
	${GO} test -v -race -run Integration ./test/integration/...

# Run end-to-end tests
test-e2e:
	@echo "Running e2e tests..."
	${GO} test -v -race ./test/e2e/...

# Generate coverage report
coverage:
	@echo "Generating coverage report..."
	${GO} test -coverprofile=coverage.out ./...
	${GO} tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Run benchmarks
bench:
	@echo "Running benchmarks..."
	${GO} test -bench=. -benchmem ./...

# Run the formal mounted-gateway benchmark suite
benchmark-suite:
	@echo "Running Bluestone benchmark suite..."
	./scripts/run_benchmark_suite.sh

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@rm -f coverage.out coverage.html
	@${GO} clean

# Install dependencies
install:
	@echo "Installing dependencies..."
	${GO} mod download
	${GO} mod tidy

# Install as a Linux systemd service
install-service:
	@echo "Installing ${BINARY_NAME} as a Linux systemd service..."
	./scripts/install-linux-service.sh --build

# Run the application
run:
	@echo "Running ${BINARY_NAME}..."
	${GO} run ./cmd/bluestone --config configs/config.yaml

# Run with development config
run-dev:
	@echo "Running ${BINARY_NAME} in development mode..."
	${GO} run ./cmd/bluestone --config configs/config.example.yaml

# Format code
fmt:
	@echo "Formatting code..."
	${GO} fmt ./...

# Run linter
lint:
	@echo "Running linter..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed" && exit 1)
	golangci-lint run ./...

# Vet code
vet:
	@echo "Vetting code..."
	${GO} vet ./...

# Build Docker image
docker-build:
	@echo "Building Docker image..."
	docker build -t ${DOCKER_IMAGE}:${VERSION} -f deployments/docker/Dockerfile .

# Push Docker image
docker-push:
	@echo "Pushing Docker image..."
	docker push ${DOCKER_IMAGE}:${VERSION}

# Run Docker container
docker-run:
	@echo "Running Docker container..."
	docker run -p 2049:2049 -p 8080:8080 -p 8081:8081 \
		-v $(PWD)/configs:/etc/bluestone \
		-v $(PWD)/cache:/var/cache/bluestone \
		${DOCKER_IMAGE}:${VERSION}

# Deploy to Kubernetes
k8s-deploy:
	@echo "Deploying to Kubernetes..."
	kubectl apply -f deployments/kubernetes/

# Remove from Kubernetes
k8s-delete:
	@echo "Removing from Kubernetes..."
	kubectl delete -f deployments/kubernetes/

# Show Kubernetes status
k8s-status:
	@echo "Kubernetes status..."
	kubectl get all -l app=bluestone

# View logs
k8s-logs:
	@echo "Viewing logs..."
	kubectl logs -l app=bluestone -f

# Generate mocks for testing
mocks:
	@echo "Generating mocks..."
	@which mockgen > /dev/null || (echo "mockgen not installed. Run: go install github.com/golang/mock/mockgen@latest" && exit 1)
	mockgen -source=pkg/types/types.go -destination=test/mocks/types_mock.go -package=mocks

# Show help
help:
	@echo "Available targets:"
	@echo "  make build           - Build the binary"
	@echo "  make build-all       - Build for multiple platforms"
	@echo "  make test            - Run all tests"
	@echo "  make test-unit       - Run unit tests"
	@echo "  make test-integration - Run integration tests"
	@echo "  make test-e2e        - Run e2e tests"
	@echo "  make coverage        - Generate coverage report"
	@echo "  make bench           - Run benchmarks"
	@echo "  make benchmark-suite - Run mounted gateway benchmark suite"
	@echo "  make clean           - Clean build artifacts"
	@echo "  make install         - Install dependencies"
	@echo "  make install-service - Install as a Linux systemd service"
	@echo "  make run             - Run the application"
	@echo "  make run-dev         - Run in development mode"
	@echo "  make fmt             - Format code"
	@echo "  make lint            - Run linter"
	@echo "  make vet             - Vet code"
	@echo "  make docker-build    - Build Docker image"
	@echo "  make docker-push     - Push Docker image"
	@echo "  make docker-run      - Run Docker container"
	@echo "  make k8s-deploy      - Deploy to Kubernetes"
	@echo "  make k8s-delete      - Remove from Kubernetes"
	@echo "  make k8s-status      - Show Kubernetes status"
	@echo "  make k8s-logs        - View logs"
	@echo "  make mocks           - Generate mocks for testing"
	@echo "  make help            - Show this help message"
