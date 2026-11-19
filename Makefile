.PHONY: build test install install-service logs

# Install to a fixed per-user location, independent of GOBIN/GOPATH (version
# managers like mise point GOBIN at a per-Go-version directory).
BINDIR ?= $(HOME)/.local/bin

build:
	go build -o bin/shelf ./cmd/shelf

test:
	go vet ./... && go test ./...

install:
	mkdir -p $(BINDIR)
	GOBIN=$(BINDIR) go install ./cmd/shelf
	@echo "installed $(BINDIR)/shelf"

install-service: install
	mkdir -p ~/.config/systemd/user
	cp deploy/shelf.service ~/.config/systemd/user/shelf.service
	systemctl --user daemon-reload
	systemctl --user enable shelf
	systemctl --user restart shelf
	@echo "Tip: 'loginctl enable-linger $$USER' keeps it running when you're logged out."

logs:
	journalctl --user -u shelf -f
