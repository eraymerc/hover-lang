# ─────────────────────────────────────────────────────────────────────────────
# HOVER COMPILER & RUNTIME — Release Makefile
#
# Releases ship the C++ runtime as SOURCE, not as a prebuilt library: the
# user's own Zig compiles it via `hover --setup` (see runtimebuild.go). That
# makes the toolchain that builds the runtime and the one that builds
# sim.cpp the same by construction, which is the only way to be sure their
# C++ ABIs agree.
#
# The practical consequence for this Makefile is that packaging a platform
# is just a `go build` — there is no C++ cross-compilation here at all, and
# so no target triples, no per-platform object dirs, and no vendored Zig.
# Adding a platform is one row in PLATFORMS plus its GOOS/GOARCH, which is
# why Linux, Windows and the BSDs are all shipped.
#
# Targets:
#   make <platform>     → Package a single platform, e.g. `make linux-aarch64`
#   make all            → Package every platform into releases/$(VERSION)/
#   make check-runtime  → Compile the C++ runtime locally with warnings on.
#                         Not needed to package; run it in CI so runtime
#                         breakage surfaces here rather than on a user's
#                         machine at `hover --setup` time.
#   make clean          → Remove build artifacts, staging dirs and releases/
#   make help           → List targets
#
# make all VERSION=v1.2.3   (VERSION defaults to v0.0.0-dev if unset)
# ─────────────────────────────────────────────────────────────────────────────

# ── CONFIGURATION VARIABLES ───────────────────────────────────────────────────

# Host Zig — used only by `make check-runtime`. Packaging does not need Zig.
ZIG ?= zig

# Zig version stamped into Windows binaries as the one to download when no
# local Zig is found (see zigfetch.go). Windows has no package-manager
# convention to lean on, so `hover --setup` fetches this version itself.
# Derived from the host Zig by default; ziglang.org only serves the CURRENT
# master snapshot, so if you track master keep your host Zig current, or
# override with a tagged release (`make all ZIG_VERSION=0.16.0`) for a pin
# that never goes stale.
ZIG_VERSION ?= $(shell $(ZIG) version)

VERSION ?= v0.0.0-dev

STANDARD_LIB := stdlib
RUNTIME_DIR  := runtime

# ── PLATFORM MATRIX ────────────────────────────────────────────────────────────
# Only GOOS/GOARCH: the C++ side is built on the user's machine, natively.
#
# Because packaging is now just `go build`, a platform costs two lines here
# rather than a cross-compiled C++ toolchain — which is what makes the BSDs
# practical. A platform ships if it clears all THREE gates:
#
#   1. Go builds the CLI:  GOOS=<os> GOARCH=<arch> go build -o /dev/null .
#   2. Zig builds the runtime, since the user compiles it locally:
#      zig c++ -std=c++17 -Iruntime -Iruntime/Eigen -target <arch>-<os> \
#          -c runtime/mna/engine.cpp -o /tmp/t.o
#   3. The user can OBTAIN Zig there — i.e. ziglang.org's download index
#      lists an <arch>-<os> host build. Without it `hover --setup` has no
#      compiler to run and the package is inert.
#
# Gate 3 is the one that is easy to forget, and it is why three otherwise-
# viable Go/Zig targets are absent: freebsd/386, openbsd/386 and
# openbsd/ppc64 have no official Zig build (Zig ships powerpc64le-freebsd,
# which is a different platform). DragonFly and Solaris fail gate 2.

PLATFORMS := linux-x86_64 linux-aarch64 linux-x86 linux-riscv64 \
             windows-x86_64 windows-aarch64 windows-x86 \
             freebsd-x86_64 freebsd-aarch64 freebsd-arm \
             netbsd-x86_64 netbsd-aarch64 netbsd-arm netbsd-x86 \
             openbsd-x86_64 openbsd-aarch64 openbsd-arm openbsd-riscv64

linux-x86_64_GOOS      := linux
linux-x86_64_GOARCH    := amd64

linux-aarch64_GOOS     := linux
linux-aarch64_GOARCH   := arm64

linux-x86_GOOS         := linux
linux-x86_GOARCH       := 386

linux-riscv64_GOOS     := linux
linux-riscv64_GOARCH   := riscv64

windows-x86_64_GOOS    := windows
windows-x86_64_GOARCH  := amd64

windows-aarch64_GOOS   := windows
windows-aarch64_GOARCH := arm64

windows-x86_GOOS       := windows
windows-x86_GOARCH     := 386

freebsd-x86_64_GOOS    := freebsd
freebsd-x86_64_GOARCH  := amd64

freebsd-aarch64_GOOS   := freebsd
freebsd-aarch64_GOARCH := arm64

freebsd-arm_GOOS       := freebsd
freebsd-arm_GOARCH     := arm

netbsd-x86_64_GOOS     := netbsd
netbsd-x86_64_GOARCH   := amd64

netbsd-aarch64_GOOS    := netbsd
netbsd-aarch64_GOARCH  := arm64

netbsd-arm_GOOS        := netbsd
netbsd-arm_GOARCH      := arm

netbsd-x86_GOOS        := netbsd
netbsd-x86_GOARCH      := 386

openbsd-x86_64_GOOS    := openbsd
openbsd-x86_64_GOARCH  := amd64

openbsd-aarch64_GOOS   := openbsd
openbsd-aarch64_GOARCH := arm64

openbsd-arm_GOOS       := openbsd
openbsd-arm_GOARCH     := arm

openbsd-riscv64_GOOS   := openbsd
openbsd-riscv64_GOARCH := riscv64

# ── DEFAULT TARGETS ───────────────────────────────────────────────────────────

.PHONY: all clean help check-runtime $(PLATFORMS) linux windows

all: $(PLATFORMS)

# Back-compat aliases for the old two-platform names.
linux: linux-x86_64
windows: windows-x86_64

# ── PER-PLATFORM PACKAGE TEMPLATE ─────────────────────────────────────────────

define PLATFORM_TEMPLATE

EXE_EXT_$(1) := $$(if $$(filter windows,$$($(1)_GOOS)),.exe,)
DIR_$(1)     := releases/$$(VERSION)/hover_$$(subst -,_,$$(VERSION))_$$(subst -,_,$(1))

.PHONY: $(1)
$(1):
	@echo "\n[$(1)] Assembling Hover Standalone..."
	@rm -rf $$(DIR_$(1))
	@mkdir -p $$(DIR_$(1))
	cp -r $(STANDARD_LIB) $$(DIR_$(1))/stdlib
	# Runtime ships complete, sources included — `hover --setup` compiles it.
	cp -r $(RUNTIME_DIR) $$(DIR_$(1))/runtime
	rm -rf $$(DIR_$(1))/runtime/build
	GOOS=$$($(1)_GOOS) GOARCH=$$($(1)_GOARCH) go build -ldflags "-X main.zigVersion=$$(ZIG_VERSION)" -o $$(DIR_$(1))/hover$$(EXE_EXT_$(1)) .
	# Attribution for the bundled third-party code (Eigen, klauspost/compress,
	# and Zig in the Windows packages). BSD-3 and Apache-2.0 both require the
	# notice to accompany a BINARY redistribution, which a release zip is.
	cp LICENSE THIRD_PARTY_LICENSES.md $$(DIR_$(1))/
	# `hpm` is the same binary, dispatched on argv[0] (see invokedAsHPM in
	# main.go), so `hpm install foo` and `hover hpm install foo` are the same
	# words in the same order. Windows gets a batch shim instead: symlinks
	# there need either developer mode or elevation, and zip does not carry
	# them portably anyway.
	$$(if $$(filter windows,$$($(1)_GOOS)),\
	  printf '@echo off\r\n"%%~dp0hover.exe" hpm %%*\r\n' > $$(DIR_$(1))/hpm.bat,\
	  ln -sf hover $$(DIR_$(1))/hpm)
	cd releases/$$(VERSION) && zip -r --symlinks hover-$$(VERSION)-$(1).zip $$(notdir $$(DIR_$(1)))
	@echo "[$(1)] Release ready: releases/$$(VERSION)/hover-$$(VERSION)-$(1).zip"

endef

$(foreach p,$(PLATFORMS),$(eval $(call PLATFORM_TEMPLATE,$(p))))

# ── RUNTIME VERIFICATION (CI) ─────────────────────────────────────────────────
# Mirrors what `hover --setup` does on a user's machine, but with warnings
# enabled: users see -w, maintainers should not.

CHECK_DIR  := $(RUNTIME_DIR)/build/check
CHECK_SRCS := $(shell find $(RUNTIME_DIR) -name '*.cpp' -not -path '$(RUNTIME_DIR)/build/*')
CHECK_OBJS := $(patsubst $(RUNTIME_DIR)/%.cpp,$(CHECK_DIR)/%.o,$(CHECK_SRCS))

$(CHECK_DIR)/%.o: $(RUNTIME_DIR)/%.cpp
	@mkdir -p $(dir $@)
	$(ZIG) c++ -std=c++17 -O3 -Wall -Wextra \
		-I$(RUNTIME_DIR) -I$(RUNTIME_DIR)/Eigen -MMD -MP -c $< -o $@

check-runtime: $(CHECK_OBJS)
	$(ZIG) ar rcs $(CHECK_DIR)/libhover_runtime.a $(CHECK_OBJS)
	@echo "[Check] Runtime compiles cleanly: $(CHECK_DIR)/libhover_runtime.a"

-include $(CHECK_OBJS:.o=.d)

# ── UTILITIES ─────────────────────────────────────────────────────────────────

clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(RUNTIME_DIR)/build
	rm -rf releases
	rm -rf hover_linux hover_win
	rm -f hover-linux-x64.zip hover-windows-x64.zip

help:
	@echo "Hover Release Build System"
	@echo "  make <platform>    - Package one platform:"
	@echo "                       $(PLATFORMS)"
	@echo "  make all           - Package every platform into releases/\$$(VERSION)/"
	@echo "  make check-runtime - Compile the C++ runtime locally (CI check)"
	@echo "  make clean         - Remove build artifacts"
	@echo ""
	@echo "  VERSION=$(VERSION) (override with VERSION=v1.2.3)"
	@echo "  Releases ship runtime SOURCE; users run 'hover --setup' to build"
	@echo "  it with their own Zig. Packaging needs Go only, not Zig."
