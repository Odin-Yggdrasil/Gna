.PHONY: build gna rdp test install deb clean

VERSION := $(shell cat VERSION)

# Gná (Go, binaire statique) + passerelle RDP gna-rdp (Rust)
build: gna rdp

gna:
	go mod tidy
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/gna ./cmd/gna

# Passerelle RDP : compilée si Rust ≥ 1.89 est présent (rustup), sinon le binaire
# précompilé fourni (Linux x86-64, glibc ≥ 2.34 : Debian 12 et 13, Ubuntu 22.04 et plus) est utilisé.
rdp:
	@mkdir -p bin
	@if command -v cargo >/dev/null 2>&1 && cargo --version | awk '{split($$2,v,"."); exit !(v[1]>1 || (v[1]==1 && v[2]>=89))}'; then \
		echo "Compilation de la passerelle RDP (gna-rdp)…"; \
		cd rdpbridge && cargo build --release --locked && cp target/release/gna-rdp ../bin/gna-rdp; \
	elif [ "$$(uname -m)" = "x86_64" ] && [ -f deploy/prebuilt/gna-rdp-linux-amd64 ]; then \
		echo "Rust ≥ 1.89 absent : utilisation de la passerelle RDP précompilée."; \
		install -m 0755 deploy/prebuilt/gna-rdp-linux-amd64 bin/gna-rdp; \
	else \
		echo "Attention : passerelle RDP non disponible (installer Rust avec https://rustup.rs). Le RDP sera désactivé."; \
	fi

test:
	go test ./...
	cd rdpbridge && cargo test

install: build
	sudo ./deploy/install.sh

# Paquet Debian du serveur : dist/gna_<version>_amd64.deb
deb: build
	./deploy/deb/build.sh $(VERSION)

clean:
	rm -rf bin dist
