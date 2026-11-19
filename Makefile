.PHONY: build test install install-service logs

build:
	go build -o bin/shelf ./cmd/shelf

test:
	go vet ./... && go test ./...

install:
	go install ./cmd/shelf

install-service: install
	mkdir -p ~/.config/systemd/user
	cp deploy/shelf.service ~/.config/systemd/user/shelf.service
	systemctl --user daemon-reload
	systemctl --user enable --now shelf
	@echo "Tip: 'loginctl enable-linger $$USER' keeps it running when you're logged out."

logs:
	journalctl --user -u shelf -f
