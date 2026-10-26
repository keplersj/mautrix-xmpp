# goolm: pure-Go Olm for end-to-bridge encryption (no libolm needed).
GO_TAGS := goolm
LDFLAGS := -s -w -X main.Commit=$(shell git rev-parse HEAD 2>/dev/null) \
	-X 'main.BuildTime=$(shell date -Iseconds)' \
	-X maunium.net/go/mautrix.GoModVersion=$(shell awk '$$1 == "maunium.net/go/mautrix" { print $$2 }' go.mod)
COMPOSE := docker compose -f integration/docker-compose.yml
PROMTOOL := docker run --rm -v "$(CURDIR)/contrib/grafana:/work:ro" -w /work \
	--entrypoint promtool prom/prometheus:v3.15.1

.PHONY: build test lint integration integration-down image check-grafana

build:
	go build -tags $(GO_TAGS) -ldflags "$(LDFLAGS)" -o mautrix-xmpp ./cmd/mautrix-xmpp

test:
	go vet -tags $(GO_TAGS),integration ./...
	go test -tags $(GO_TAGS) ./...

lint:
	golangci-lint run ./...

# End-to-end test against Prosody and Synapse in Docker Compose.
integration:
	$(COMPOSE) build bridge
	$(COMPOSE) up --abort-on-container-exit --exit-code-from tester; \
		status=$$?; $(COMPOSE) down -v; exit $$status

integration-down:
	$(COMPOSE) down -v

image:
	docker buildx build --platform linux/amd64,linux/arm64 -t mautrix-xmpp:dev .

# Parse the dashboard's queries and the alert rules, and run the alert tests.
check-grafana:
	python3 contrib/grafana/dashboard_queries.py contrib/grafana/dashboard.json \
		> contrib/grafana/.dashboard-queries.yaml
	$(PROMTOOL) check rules alerts.yaml .dashboard-queries.yaml
	$(PROMTOOL) test rules alerts_test.yaml
