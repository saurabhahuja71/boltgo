VERSION ?= 1.1.15
BINARY  := bolt
MODULE  := ./cmd/agenterm
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install tidy test dist release-local clean \
	container container-run help

help:
	@echo "Native:"
	@echo "  make build          Build ./bolt for this OS"
	@echo "  make install        go install to \$$GOPATH/bin"
	@echo "  make dist           Cross-compile release binaries into dist/"
	@echo "  make release-local  dist + print checksums (tag/push separately)"
	@echo ""
	@echo "Container:"
	@echo "  make container      podman/docker build -t agenterm:\$(VERSION)"
	@echo "  make container-run  Interactive TUI (host network)"
	@echo ""
	@echo "  VERSION=$(VERSION)"

## --- Native ---

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) $(MODULE)
	ln -sf $(BINARY) bolt-s1
	ln -sf $(BINARY) bolt-s2
	ln -sf $(BINARY) bolt-s3

install:
	CGO_ENABLED=0 go install -trimpath -ldflags="$(LDFLAGS)" $(MODULE)

tidy:
	go mod tidy

## Cross-compile artifacts for GitHub Release
dist: clean-dist
	@mkdir -p dist
	@set -e; \
	for pair in \
		linux/amd64 \
		linux/arm64 \
		darwin/amd64 \
		darwin/arm64 \
		windows/amd64 \
	; do \
		os=$${pair%/*}; arch=$${pair#*/}; \
		ext=""; [ "$$os" = windows ] && ext=".exe"; \
		out="dist/$(BINARY)-$$os-$$arch$$ext"; \
		echo "→ $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags="$(LDFLAGS)" -o "$$out" $(MODULE); \
		(cd dist && sha256sum "$$(basename $$out)" > "$$(basename $$out).sha256"); \
	done
	@ls -la dist/

release-local: dist
	@echo ""
	@echo "Local artifacts ready in dist/ for v$(VERSION)"
	@echo "Create tag and push to publish via GitHub Actions:"
	@echo "  git tag v$(VERSION) && git push origin v$(VERSION)"

clean-dist:
	rm -rf dist

clean: clean-dist
	rm -f $(BINARY)

## --- Container ---

ENGINE ?= $(shell command -v podman >/dev/null && echo podman || echo docker)

container:
	$(ENGINE) build -t agenterm:$(VERSION) -t agenterm:latest \
		--build-arg VERSION=$(VERSION) .

container-run:
	$(ENGINE) run --rm -it --network=host \
		-e AGENTERM_BASE_URL=$${AGENTERM_BASE_URL:-http://127.0.0.1:11434/v1} \
		-e AGENTERM_MODEL=$${AGENTERM_MODEL:-qwen2.5-coder:32b} \
		-v "$$HOME/.agenterm:/home/agenterm/.agenterm:Z" \
		agenterm:$(VERSION)
