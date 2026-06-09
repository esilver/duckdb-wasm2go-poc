#include <stdexcept>
#include <string>
#include <cstdint>
extern "C" int probe(int x) {
    try {
        if (x % 2) throw std::runtime_error("odd");
        return x * 2;
    } catch (const std::exception &e) {
        return -1;
    }
}
