#pragma once

// ─────────────────────────────────────────────────────────────────────────────
// HOVER RUNTIME — SINGLE INCLUDE
// Generated sim.cpp includes only this file.
// ─────────────────────────────────────────────────────────────────────────────

#include "mna/system.hpp"
#include "mna/matrices.hpp"
#include "mna/engine.hpp"
#include "mna/api.hpp"
#include "vm/logger.hpp"
#include "vm/snapshot.hpp"
#include "vm/zcd.hpp"
#include "vm/vm.hpp"
#include "solvers/euler_fixed.hpp"
#include "solvers/euler_adaptive.hpp"
#include "solvers/gauss_siedel.hpp"
#include "solvers/trapezoidal.hpp"
#include "solvers/trapezoidal_fixed.hpp"
#include "solvers/bdf2.hpp"
#include "solvers/ndf2.hpp"