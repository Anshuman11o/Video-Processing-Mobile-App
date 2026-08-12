# DayReel Development Makefile

.PHONY: dev-up dev-down dev-logs dev-reset test-infra help

# Default target
help:
	@echo "DayReel Development Commands"
	@echo "============================"
	@echo ""
	@echo "Infrastructure:"
	@echo "  make dev-up      - Start development infrastructure (LocalStack, Redis)"
	@echo "  make dev-down    - Stop development infrastructure"
	@echo "  make dev-logs    - View container logs (follow mode)"
	@echo "  make dev-reset   - Reset all data and restart containers"
	@echo "  make test-infra  - Test infrastructure connectivity and resources"
	@echo ""

# Start development infrastructure
dev-up:
	@echo "Starting development infrastructure..."
	@cd infra && docker compose up -d
	@echo ""
	@echo "Waiting for services to be ready..."
	@timeout 90 bash -c 'until curl -sf http://localhost:4566/_localstack/health 2>/dev/null | grep -q "running"; do sleep 2; done' && echo "LocalStack ready!" || echo "LocalStack timeout"
	@timeout 30 bash -c 'until docker exec dayreel-redis redis-cli ping 2>/dev/null | grep -q PONG; do sleep 1; done' && echo "Redis ready!" || echo "Redis timeout"
	@echo ""
	@echo "Development infrastructure is running!"
	@echo "  LocalStack: http://localhost:4566"
	@echo "  Redis:      localhost:6379"

# Stop development infrastructure
dev-down:
	@echo "Stopping development infrastructure..."
	@cd infra && docker compose down
	@echo "Infrastructure stopped."

# View logs
dev-logs:
	@cd infra && docker compose logs -f

# Reset all data
dev-reset:
	@echo "Resetting development infrastructure..."
	@cd infra && docker compose down -v
	@echo "Removed containers and volumes."
	@echo ""
	@make dev-up

# Test infrastructure
test-infra:
	@echo "Testing infrastructure..."
	@echo ""
	@echo "=== Container Status ==="
	@docker ps --filter "name=dayreel" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
	@echo ""
	@echo "=== LocalStack Health ==="
	@curl -sf http://localhost:4566/_localstack/health 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "LocalStack not responding"
	@echo ""
	@echo "=== S3 Buckets ==="
	@aws --endpoint-url=http://localhost:4566 s3 ls 2>/dev/null || echo "Failed to list buckets"
	@echo ""
	@echo "=== SQS Queues ==="
	@aws --endpoint-url=http://localhost:4566 sqs list-queues 2>/dev/null || echo "Failed to list queues"
	@echo ""
	@echo "=== DynamoDB Tables ==="
	@aws --endpoint-url=http://localhost:4566 dynamodb list-tables 2>/dev/null || echo "Failed to list tables"
	@echo ""
	@echo "=== Redis Connectivity ==="
	@docker exec dayreel-redis redis-cli ping 2>/dev/null || echo "Redis not responding"
	@echo ""
	@echo "Infrastructure test complete."
