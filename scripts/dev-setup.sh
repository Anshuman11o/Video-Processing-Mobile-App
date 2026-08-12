#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "=========================================="
echo "DayReel Development Environment Setup"
echo "=========================================="
echo ""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_status() {
    echo -e "${GREEN}[OK]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check prerequisites
echo "Checking prerequisites..."

# Check Docker
if ! command -v docker &> /dev/null; then
    print_error "Docker is not installed. Please install Docker Desktop."
    exit 1
fi

if ! docker info &> /dev/null; then
    print_error "Docker is not running. Please start Docker Desktop."
    exit 1
fi
print_status "Docker is running"

# Check Docker Compose
if ! command -v docker-compose &> /dev/null && ! docker compose version &> /dev/null; then
    print_error "Docker Compose is not installed."
    exit 1
fi
print_status "Docker Compose is available"

# Check AWS CLI
if ! command -v aws &> /dev/null; then
    print_warning "AWS CLI is not installed. Install it for local testing commands."
else
    print_status "AWS CLI is installed"
fi

# Check if ports are available
check_port() {
    if lsof -i ":$1" &> /dev/null; then
        print_error "Port $1 is already in use"
        return 1
    fi
    return 0
}

echo ""
echo "Checking port availability..."
PORTS_OK=true
if ! check_port 4566; then
    print_error "Port 4566 (LocalStack) is in use"
    PORTS_OK=false
fi
if ! check_port 6379; then
    print_error "Port 6379 (Redis) is in use"
    PORTS_OK=false
fi

if [ "$PORTS_OK" = false ]; then
    echo ""
    print_error "Some required ports are in use. Please free them before continuing."
    exit 1
fi
print_status "All required ports are available"

# Setup .env file
echo ""
echo "Setting up environment..."
if [ ! -f "$PROJECT_ROOT/.env" ]; then
    if [ -f "$PROJECT_ROOT/.env.example" ]; then
        cp "$PROJECT_ROOT/.env.example" "$PROJECT_ROOT/.env"
        print_status "Created .env from .env.example"
    else
        print_warning "No .env.example found, skipping .env creation"
    fi
else
    print_status ".env file already exists"
fi

# Start Docker Compose
echo ""
echo "Starting Docker Compose services..."
cd "$PROJECT_ROOT/infra"

# Use docker compose (v2) if available, otherwise docker-compose (v1)
if docker compose version &> /dev/null; then
    COMPOSE_CMD="docker compose"
else
    COMPOSE_CMD="docker-compose"
fi

$COMPOSE_CMD up -d

# Wait for LocalStack to be healthy
echo ""
echo "Waiting for LocalStack to be ready..."
TIMEOUT=90
ELAPSED=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    if curl -sf http://localhost:4566/_localstack/health 2>/dev/null | grep -q "running"; then
        print_status "LocalStack is ready"
        break
    fi
    echo "  Waiting for LocalStack... (${ELAPSED}s/${TIMEOUT}s)"
    sleep 3
    ELAPSED=$((ELAPSED + 3))
done

if [ $ELAPSED -ge $TIMEOUT ]; then
    print_error "LocalStack failed to start within ${TIMEOUT} seconds"
    exit 1
fi

# Wait for Redis to be healthy
echo ""
echo "Waiting for Redis to be ready..."
TIMEOUT=30
ELAPSED=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    if docker exec dayreel-redis redis-cli ping 2>/dev/null | grep -q PONG; then
        print_status "Redis is ready"
        break
    fi
    sleep 1
    ELAPSED=$((ELAPSED + 1))
done

if [ $ELAPSED -ge $TIMEOUT ]; then
    print_error "Redis failed to start within ${TIMEOUT} seconds"
    exit 1
fi

# Display status
echo ""
echo "=========================================="
echo "Development environment is ready!"
echo "=========================================="
echo ""
echo "Services:"
echo "  - LocalStack: http://localhost:4566"
echo "  - Redis:      localhost:6379"
echo ""
echo "AWS Resources (via LocalStack):"
echo "  - S3 Buckets: dayreel-raw-uploads, dayreel-processed-videos, dayreel-thumbnails"
echo "  - SQS Queues: dayreel-video-processing, dayreel-notifications"
echo "  - DynamoDB:   dayreel-videos"
echo ""
echo "Useful commands:"
echo "  make dev-logs    - View container logs"
echo "  make dev-down    - Stop containers"
echo "  make dev-reset   - Reset all data"
echo "  make test-infra  - Test infrastructure"
echo ""
