LIBDAVE_VERSION := v1.1.0

tools:
	go install github.com/air-verse/air@latest

# installs libdave (Discord DAVE E2EE voice encryption, used via
# github.com/disgoorg/godave/libdave) for local CGO builds on Arch.
# add the printed PKG_CONFIG_PATH export to your shell profile.
libdave:
	sudo pacman -S --needed --noconfirm curl unzip
	curl -fsSL https://raw.githubusercontent.com/disgoorg/godave/master/scripts/libdave_install.sh | sh -s $(LIBDAVE_VERSION)

build:
	go mod tidy
	docker build -t dbot .

compose:
	DBOT_TAG=$(filter-out $@,$(MAKECMDGOALS)) docker compose up --build -d && docker logs dbot-dbot-1 -f

%:
	@:
