BIN      := $(HOME)/.local/bin/keybar
APPS     := $(HOME)/.local/share/applications
ICONS    := $(HOME)/.local/share/icons/hicolor/scalable/apps
UNITDIR  := $(HOME)/.config/systemd/user

.PHONY: build install uninstall restart logs keycodes

build:
	go build -o keybar .

keycodes:
	./gen_keycodes.sh

install: build
	install -Dm755 keybar $(BIN)
	install -Dm644 web/logo.svg $(ICONS)/keybar.svg
	install -Dm644 packaging/keybar.desktop $(APPS)/keybar.desktop
	install -Dm644 packaging/keybar.service $(UNITDIR)/keybar.service
	-update-desktop-database $(APPS) 2>/dev/null
	-gtk-update-icon-cache -q $(HOME)/.local/share/icons/hicolor 2>/dev/null
	systemctl --user daemon-reload
	systemctl --user enable keybar.service
	systemctl --user restart keybar.service

restart:
	systemctl --user restart keybar.service

logs:
	tail -f $(HOME)/.cache/keybar/keybar.log

uninstall:
	-systemctl --user disable --now keybar.service
	rm -f $(BIN) $(ICONS)/keybar.svg $(APPS)/keybar.desktop $(UNITDIR)/keybar.service
	systemctl --user daemon-reload
