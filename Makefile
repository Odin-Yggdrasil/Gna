.PHONY: build app test install deb clean

# Raccourcis vers scripts/ et packaging/ (voir README).
build:
	./scripts/build.sh

app:
	./scripts/build.sh --app

test:
	cd server && go test ./...
	cd server/rdpbridge && cargo test

install: build
	sudo ./packaging/install.sh

# Paquet Debian du serveur : clients/installateurs/gna-server_<version>_amd64.deb
deb: build
	./packaging/deb/build-deb.sh

clean:
	rm -rf dist/linux
