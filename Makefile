# ─────────────────────────────────────────────────────────────────────────────
# HOVER COMPILER & RUNTIME — Release Makefile
# 
# Targets:
#   make linux    → Cross-compiles runtime & Go CLI for Linux, creates zip
#   make windows  → Cross-compiles runtime & Go CLI for Windows, creates zip
#   make all      → Builds both standalone releases
#   make clean    → Removes all build artifacts and standalone folders
# ─────────────────────────────────────────────────────────────────────────────

# ── CONFIGURATION VARIABLES ───────────────────────────────────────────────────

# Host Zig executable (Assuming Zig is in your PATH to build the project)
ZIG ?= zig

# Vendor Paths (The compilers that get shipped to the user)
ZIG_LINUX_VENDOR := toolchain/zig-x86_64-linux
ZIG_WIN_VENDOR   := toolchain/zig-x86_64-win

# ── RUNTIME SOURCES ───────────────────────────────────────────────────────────

RUNTIME_DIR := runtime
EIGEN_INC   := -I$(RUNTIME_DIR)/Eigen -I/usr/include/eigen3
INCLUDES    := -I$(RUNTIME_DIR) $(EIGEN_INC)
CXXFLAGS    := -std=c++17 -O3 -Wall -Wextra $(INCLUDES)

SRCS := \
    $(RUNTIME_DIR)/mna/system.cpp     \
    $(RUNTIME_DIR)/mna/matrices.cpp   \
    $(RUNTIME_DIR)/mna/engine.cpp     \
    $(RUNTIME_DIR)/mna/api.cpp        \
    $(RUNTIME_DIR)/vm/logger.cpp      \
    $(RUNTIME_DIR)/vm/snapshot.cpp    \
    $(RUNTIME_DIR)/vm/zcd.cpp         \
    $(RUNTIME_DIR)/vm/vm.cpp          \
    $(RUNTIME_DIR)/solvers/euler_fixed.cpp \
	$(RUNTIME_DIR)/solvers/gauss_siedel.cpp   \
    $(RUNTIME_DIR)/solvers/euler_adaptive.cpp \
    $(RUNTIME_DIR)/solvers/trapezoidal.cpp    \
	$(RUNTIME_DIR)/solvers/trapezoidal_fixed.cpp    \
    $(RUNTIME_DIR)/solvers/bdf2.cpp     \
	$(RUNTIME_DIR)/solvers/ndf2.cpp           



# ── LINUX TARGET ──────────────────────────────────────────────────────────────

BUILD_LINUX  := $(RUNTIME_DIR)/build/linux
OBJS_LINUX   := $(patsubst $(RUNTIME_DIR)/%.cpp, $(BUILD_LINUX)/%.o, $(SRCS))
LIB_LINUX    := $(BUILD_LINUX)/libhover_runtime.a
DIR_LINUX    := hover_linux

CXX_LINUX    := $(ZIG) c++ -target x86_64-linux-gnu
AR_LINUX     := $(ZIG) ar rcs

# ── WINDOWS TARGET ────────────────────────────────────────────────────────────

BUILD_WIN    := $(RUNTIME_DIR)/build/windows
OBJS_WIN     := $(patsubst $(RUNTIME_DIR)/%.cpp, $(BUILD_WIN)/%.o, $(SRCS))
# Updated to use the standard Windows .lib extension and naming convention
LIB_WIN      := $(BUILD_WIN)/hover_runtime.lib
DIR_WIN      := hover_win

CXX_WIN      := $(ZIG) c++ -target x86_64-windows-gnu
AR_WIN       := $(ZIG) ar rcs

# ── DEFAULT TARGETS ───────────────────────────────────────────────────────────

.PHONY: all linux windows clean prep-linux prep-win help

all: linux windows

# ── C++ RUNTIME BUILD RULES ───────────────────────────────────────────────────

# Linux Object Files
$(BUILD_LINUX)/%.o: $(RUNTIME_DIR)/%.cpp
	@mkdir -p $(dir $@)
	$(CXX_LINUX) $(CXXFLAGS) -MMD -MP -c $< -o $@

# Linux Static Library
$(LIB_LINUX): $(OBJS_LINUX)
	@mkdir -p $(dir $@)
	$(AR_LINUX) $@ $^
	@echo "[Build] Linux runtime built: $@"

# Windows Object Files
$(BUILD_WIN)/%.o: $(RUNTIME_DIR)/%.cpp
	@mkdir -p $(dir $@)
	$(CXX_WIN) $(CXXFLAGS) -MMD -MP -c $< -o $@

# Windows Static Library
$(LIB_WIN): $(OBJS_WIN)
	@mkdir -p $(dir $@)
	$(AR_WIN) $@ $^
	@echo "[Build] Windows runtime built: $@"

# Include Dependency Files
-include $(OBJS_LINUX:.o=.d)
-include $(OBJS_WIN:.o=.d)

# ── STANDALONE PACKAGING RULES ────────────────────────────────────────────────

linux: $(LIB_LINUX)
	@echo "\n[Linux] Assembling Hover Standalone..."
	@rm -rf $(DIR_LINUX)
	@mkdir -p $(DIR_LINUX)/toolchain
	
	# 1. Copy Vendor Zig
	cp -r $(ZIG_LINUX_VENDOR) $(DIR_LINUX)/toolchain/zig
	
	# 2. Copy Runtime Headers & Library
	cp -r $(RUNTIME_DIR) $(DIR_LINUX)/runtime
	rm -rf $(DIR_LINUX)/runtime/build
	find $(DIR_LINUX)/runtime -type f -name "*.cpp" -delete
	cp $(LIB_LINUX) $(DIR_LINUX)/runtime/libhover_runtime.a
	
	# 3. Cross-Compile Go Transpiler for Linux
	GOOS=linux GOARCH=amd64 go build -o $(DIR_LINUX)/hover main.go
	
	# 4. Zip the Release
	zip -r hover-linux-x64.zip $(DIR_LINUX)
	@echo "[Linux] Release ready: hover-linux-x64.zip"

windows: $(LIB_WIN)
	@echo "\n[Windows] Assembling Hover Standalone..."
	@rm -rf $(DIR_WIN)
	@mkdir -p $(DIR_WIN)/toolchain
	
	# 1. Copy Vendor Zig
	cp -r $(ZIG_WIN_VENDOR) $(DIR_WIN)/toolchain/zig
	
	# 2. Copy Runtime Headers & Library
	cp -r $(RUNTIME_DIR) $(DIR_WIN)/runtime
	rm -rf $(DIR_WIN)/runtime/build
	find $(DIR_WIN)/runtime -type f -name "*.cpp" -delete
	cp $(LIB_WIN) $(DIR_WIN)/runtime/hover_runtime.lib
	
	# 3. Cross-Compile Go Transpiler for Windows
	GOOS=windows GOARCH=amd64 go build -o $(DIR_WIN)/hover.exe main.go
	
	# 4. Zip the Release
	zip -r hover-windows-x64.zip $(DIR_WIN)
	@echo "[Windows] Release ready: hover-windows-x64.zip"

# ── UTILITIES ─────────────────────────────────────────────────────────────────

clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(RUNTIME_DIR)/build
	rm -rf $(DIR_LINUX) $(DIR_WIN)
	rm -f hover-linux-x64.zip hover-windows-x64.zip

help:
	@echo "Hover Release Build System"
	@echo "  make linux    - Build standalone Linux release (.zip)"
	@echo "  make windows  - Build standalone Windows release (.zip)"
	@echo "  make all      - Build both releases"
	@echo "  make clean    - Remove build artifacts"