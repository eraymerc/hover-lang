#include "system.hpp"
#include <iostream>
#include <iomanip>

void System::print() const {
    std::cout << "=== HOVER SYSTEM ===\n";
    std::cout << "  Nodes: "    << num_nodes
              << ", Branches: " << num_branches
              << ", Matrix: "   << size << "x" << size << "\n";
    std::cout << "  dt: "       << std::scientific << dt << "\n";

    std::cout << "  Node map:\n";
    for (const auto &[name, idx] : node_map) {
        std::cout << "    [" << idx << "] " << name << "\n";
    }

    std::cout << "  Branch map:\n";
    for (const auto &[name, idx] : branch_map) {
        std::cout << "    [" << idx << "] " << name << "\n";
    }

    std::cout << "  Current sources: " << current_sources.size() << "\n";
    for (const auto &[name, rec] : current_sources) {
        std::cout << "    " << name
                  << ": n1=" << rec.n1
                  << " n2="  << rec.n2
                  << " last=" << std::scientific << rec.last_value << "\n";
    }

    std::cout << "  G matrix:\n" << G << "\n";
    std::cout << "  C matrix:\n" << C << "\n";
    std::cout << "  B_static:\n" << B_static.transpose() << "\n";
}