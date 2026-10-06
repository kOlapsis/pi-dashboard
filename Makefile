.PHONY: run check test lint vet cgo build build-arm64 shot boot-files deploy deploy-config logs ssh clean

BIN      := pi-dashboard
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS  := -trimpath
ADDR     ?= 127.0.0.1:8080

HOST        ?= pi-dash.local
PIUSER      ?= ben
SSH_KEY     ?= $(HOME)/.ssh/benjamin_rsa
CONFIG_YAML ?= $(HOME)/.config/pi-dashboard/config.yaml
SSHOPT      := -i $(SSH_KEY) -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new
SSH         := ssh $(SSHOPT) $(PIUSER)@$(HOST)
CHROME      ?= google-chrome
GOLANGCI    ?= $(or $(shell command -v golangci-lint 2>/dev/null),$(shell go env GOPATH)/bin/golangci-lint)

run:
	go run ./cmd/$(BIN) serve --demo --addr $(ADDR)

check: vet lint test cgo

test:
	go test -race ./...

lint:
	$(GOLANGCI) run

vet:
	go vet ./...

cgo:
	sh scripts/check-cgo.sh

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o out/$(BIN) ./cmd/$(BIN)

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o out/$(BIN)_linux_arm64 ./cmd/$(BIN)
	cd out && sha256sum $(BIN)_linux_arm64 > SHA256SUMS

shot:
	mkdir -p out/shots
	$(CHROME) --headless=new --disable-gpu --hide-scrollbars --window-size=1024,600 --screenshot=out/shots/dashboard.png http://$(ADDR)/

boot-files:
	go run ./pi/cmd/render -secrets pi/secrets.env -out out/boot
	cloud-init schema -c out/boot/user-data
	cloud-init schema -t network-config -c out/boot/network-config

deploy: build-arm64
	scp $(SSHOPT) out/$(BIN)_linux_arm64 $(PIUSER)@$(HOST):/tmp/$(BIN)
	$(SSH) 'sudo install -m0755 /tmp/$(BIN) /usr/local/bin/$(BIN) && rm /tmp/$(BIN) && sudo systemctl restart $(BIN) && systemctl is-active $(BIN)'

deploy-config:
	scp $(SSHOPT) $(CONFIG_YAML) $(PIUSER)@$(HOST):/tmp/config.yaml
	$(SSH) 'sudo install -m0600 -o root -g root /tmp/config.yaml /etc/$(BIN)/config.yaml && rm /tmp/config.yaml && sudo systemctl restart $(BIN) && systemctl is-active $(BIN)'

logs:
	$(SSH) -t 'journalctl -f -u $(BIN) -u pi-kiosk'

ssh:
	$(SSH)

clean:
	rm -rf out
