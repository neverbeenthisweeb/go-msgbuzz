build: dep
	CGO_ENABLED=0 GOOS=linux go build ./...

dep:
	@echo ">> Downloading Dependencies"
	@go mod download

test-all: test-unit test-integration

test-unit: dep
	@echo ">> Running Unit Test"
	@go test -tags=unit -count=1 -cover -covermode=atomic ./...

test-integration: dep test-infra-up
	@echo ">> Running Integration Test"
	@env RABBITMQ_URL=amqp://127.0.0.1:56723/ go test -tags=integration -count=1 -p=1 -cover -covermode=atomic ./...
	$(MAKE) test-infra-down

test-infra-up:
	$(MAKE) test-infra-down
	@echo ">> Starting Rabbit MQ"
	@docker run --name go-msgbuzz-test-rabbitmq -p 56723:5672 -d --rm rabbitmq:3
	@docker exec go-msgbuzz-test-rabbitmq sh -c 'rabbitmqctl wait /var/lib/rabbitmq/mnesia/rabbit@$$(hostname).pid'

test-infra-down:
	@echo ">> Shutting Down Rabbit MQ"
	@-docker kill go-msgbuzz-test-rabbitmq

.PHONY: build dep test-all test-unit test-integration test-infra-up test-infra-down