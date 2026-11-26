BINDIR ?= $(HOME)/.local/bin
APPDIR ?= $(HOME)/Applications
AGENTDIR ?= $(HOME)/Library/LaunchAgents
SHAREDIR ?= $(BINDIR)/../share/mi-guard
GUARD_CONFIG ?= $(HOME)/.config/mihomo-guard/mihomo-guard.yaml
GUARD_LOG ?= $(HOME)/Library/Logs/mihomo-guard.log
STABLE_CORE ?= $(shell command -v mihomo)
BUILD_DIR ?= build
MACOSX_DEPLOYMENT_TARGET ?= 12.0
ifeq ($(shell uname -s),Darwin)
RELEASE_VERSION ?= $(shell /usr/bin/plutil -extract CFBundleShortVersionString raw -o - deploy/macos/Info.plist)
endif
RELEASE_GO_TOOLCHAIN ?= go1.26.11
SWIFT_FLAGS ?= -target $(shell uname -m)-apple-macos$(MACOSX_DEPLOYMENT_TARGET)
APP_SWIFT := $(wildcard deploy/macos/*.swift)
DASHBOARD_SWIFT := $(filter-out deploy/macos/Notify.swift,$(APP_SWIFT))

.PHONY: build cli test test-macos-notify test-macos-dashboard app macos install package smoke
build: cli

cli:
	mkdir -p "$(BUILD_DIR)"
	MACOSX_DEPLOYMENT_TARGET="$(MACOSX_DEPLOYMENT_TARGET)" go build -trimpath -ldflags "-w -X main.releaseVersion=$(RELEASE_VERSION)" -o "$(BUILD_DIR)/mihomo-guard" ./cmd/mihomo-guard

test:
	go test -race ./...
	go vet ./...
	@if [ "$$(uname -s)" = Darwin ]; then $(MAKE) test-macos-notify test-macos-dashboard; fi

test-macos-notify:
	mkdir -p "$(BUILD_DIR)"
	xcrun swiftc -warnings-as-errors $(SWIFT_FLAGS) deploy/macos/NotificationSetup.swift tests/notification_setup.swift -o "$(BUILD_DIR)/notification-setup-tests"
	"$(BUILD_DIR)/notification-setup-tests"
	xcrun swiftc -warnings-as-errors $(SWIFT_FLAGS) deploy/macos/NotificationSetup.swift deploy/macos/NotificationInboxStore.swift tests/notification_inbox.swift -o "$(BUILD_DIR)/notification-inbox-tests"
	"$(BUILD_DIR)/notification-inbox-tests"

test-macos-dashboard: cli
	xcrun swiftc -warnings-as-errors $(SWIFT_FLAGS) deploy/macos/DashboardData.swift tests/dashboard_data.swift -o "$(BUILD_DIR)/dashboard-data-tests"
	"$(BUILD_DIR)/dashboard-data-tests"
	xcrun swiftc -warnings-as-errors $(SWIFT_FLAGS) deploy/macos/DashboardData.swift deploy/macos/DashboardMetrics.swift tests/dashboard_metrics.swift -o "$(BUILD_DIR)/dashboard-metrics-tests"
	"$(BUILD_DIR)/dashboard-metrics-tests"
	xcrun swiftc -warnings-as-errors $(SWIFT_FLAGS) $(DASHBOARD_SWIFT) tests/dashboard_window.swift -o "$(BUILD_DIR)/dashboard-window-tests"
	"$(BUILD_DIR)/dashboard-window-tests" "$(abspath $(BUILD_DIR)/mihomo-guard)"
	xcrun swiftc -warnings-as-errors $(SWIFT_FLAGS) deploy/macos/FlightMapData.swift deploy/macos/FlightMapGeometry.swift deploy/macos/FlightNaturalEarth.swift tests/flight_map.swift -o "$(BUILD_DIR)/flight-map-tests"
	"$(BUILD_DIR)/flight-map-tests"
	xcrun swiftc -warnings-as-errors $(SWIFT_FLAGS) deploy/macos/Flight*.swift tests/flight_panel.swift -o "$(BUILD_DIR)/flight-panel-tests"
	"$(BUILD_DIR)/flight-panel-tests"

$(BUILD_DIR)/AppIcon.icns: deploy/macos/AppIcon.png
	mkdir -p "$(BUILD_DIR)/AppIcon.iconset"
	set -e; for size in 16 32 128 256 512; do \
		sips -z $$size $$size "$<" --out "$(BUILD_DIR)/AppIcon.iconset/icon_$${size}x$${size}.png" >/dev/null; \
		retina=$$((size * 2)); \
		sips -z $$retina $$retina "$<" --out "$(BUILD_DIR)/AppIcon.iconset/icon_$${size}x$${size}@2x.png" >/dev/null; \
	done
	iconutil -c icns "$(BUILD_DIR)/AppIcon.iconset" -o "$@"

app: cli $(BUILD_DIR)/AppIcon.icns
	mkdir -p "$(BUILD_DIR)/mi-guard.app/Contents/MacOS" "$(BUILD_DIR)/mi-guard.app/Contents/Resources" "$(BUILD_DIR)/mi-guard.app/Contents/Helpers"
	cp deploy/macos/Info.plist "$(BUILD_DIR)/mi-guard.app/Contents/Info.plist"
	cp "$(BUILD_DIR)/AppIcon.icns" "$(BUILD_DIR)/mi-guard.app/Contents/Resources/AppIcon.icns"
	cp "$(BUILD_DIR)/mihomo-guard" "$(BUILD_DIR)/mi-guard.app/Contents/Helpers/mihomo-guard"
	codesign --force --sign - --timestamp=none "$(BUILD_DIR)/mi-guard.app/Contents/Helpers/mihomo-guard"
	xcrun swiftc -O $(SWIFT_FLAGS) $(APP_SWIFT) -o "$(BUILD_DIR)/mi-guard.app/Contents/MacOS/mi-guard-notify"
	/usr/libexec/PlistBuddy -c "Set :LSMinimumSystemVersion $(MACOSX_DEPLOYMENT_TARGET)" "$(BUILD_DIR)/mi-guard.app/Contents/Info.plist"
ifneq ($(strip $(RELEASE_VERSION)),)
	/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $(RELEASE_VERSION)" "$(BUILD_DIR)/mi-guard.app/Contents/Info.plist"
	/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $(RELEASE_VERSION)" "$(BUILD_DIR)/mi-guard.app/Contents/Info.plist"
endif
	codesign --force --sign - --timestamp=none "$(BUILD_DIR)/mi-guard.app"

macos: cli app
	python3 deploy/macos/launchagent.py "$(BINDIR)/mihomo-guard" "$(GUARD_CONFIG)" "$(GUARD_LOG)" > "$(BUILD_DIR)/com.lonelygo.mi-guard.plist"
	plutil -lint "$(BUILD_DIR)/com.lonelygo.mi-guard.plist" "$(BUILD_DIR)/mi-guard.app/Contents/Info.plist"

install: macos
	mkdir -p "$(BINDIR)" "$(APPDIR)" "$(AGENTDIR)" "$(SHAREDIR)" "$$(dirname "$(GUARD_LOG)")"
	install -m 755 "$(BUILD_DIR)/mihomo-guard" "$(BINDIR)/mihomo-guard"
	ditto "$(BUILD_DIR)/mi-guard.app" "$(APPDIR)/mi-guard.app"
	install -m 644 "$(BUILD_DIR)/com.lonelygo.mi-guard.plist" "$(AGENTDIR)/com.lonelygo.mi-guard.plist"
	install -m 644 share/mi-guard/mihomo-guard-default.yaml share/mi-guard/mihomo-overlay.yaml "$(SHAREDIR)/"
	@test -e "$(GUARD_CONFIG)" || "$(BINDIR)/mihomo-guard" init -c "$(GUARD_CONFIG)"

package:
	GOTOOLCHAIN="$(RELEASE_GO_TOOLCHAIN)" python3 deploy/macos/package.py --version "$(VERSION)" --deployment-target "$(MACOSX_DEPLOYMENT_TARGET)"

smoke: build
	@test -n "$(STABLE_CORE)" || { echo 'Set STABLE_CORE to your Mihomo executable, e.g. make smoke STABLE_CORE=/path/to/mihomo' >&2; exit 1; }
	python3 tests/live_core_smoke.py --core "$(STABLE_CORE)" --guard "$(CURDIR)/$(BUILD_DIR)/mihomo-guard"
