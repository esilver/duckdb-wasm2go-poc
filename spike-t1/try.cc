#include <stdexcept>

// A function that throws for odd x, returns x*2 otherwise.
static int may_throw(int x) {
    if (x % 2 != 0) {
        throw std::runtime_error("odd input");
    }
    return x * 2;
}

extern "C" int try_it(int x) {
    try {
        int r = may_throw(x);
        // Use r so it isn't optimized away; return 0 == "no throw".
        return (r >= 0) ? 0 : 0;
    } catch (const std::exception &e) {
        return 1; // catch fired
    }
}
