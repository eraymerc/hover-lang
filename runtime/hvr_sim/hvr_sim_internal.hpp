#pragma once

// Internal access to an hvr::Sim's VM — for the --hovercraft library build
// only, which steps, resets and queries the VM directly (see hvr_runtime.hpp
// and codegen's hovercraft_emit.go). A plain one-shot sim.cpp never includes
// this: hover_sim.hpp is its whole view of the runtime.

#include "../hover_sim.hpp"
#include "../vm/vm.hpp"

namespace hvr::internal {
VM &vm(Sim &sim);
}
