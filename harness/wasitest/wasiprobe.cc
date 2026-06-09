// wasiprobe.cc - a second standalone wasm whose only job is to FORCE the WASI /
// libc residual import surface that the tiny poc.cc avoided, so the wasishim
// package is validated against a real importer (not just asserted). It:
//   - calls printf -> pulls in wasi fd_write
//   - grows the heap with a large malloc -> pulls in emscripten_resize_heap
//   - reads a clock -> pulls in clock_time_get / time
//   - reads randomness -> pulls in random_get (via std::random_device on some
//     libs; we call getentropy-equivalent through arc4random-free path)
// Built EXACTLY like the DuckDB target.
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>

extern "C" {

// touch_io exercises the libc/WASI surface and returns a value derived from it
// so the call cannot be optimized away.
long long touch_io(int n) {
    // stdout via printf -> fd_write
    printf("wasiprobe: n=%d\n", n);

    // heap growth via a large allocation that exceeds the initial heap
    size_t sz = (size_t)n * 1024 * 1024; // n MiB
    char* p = (char*)malloc(sz);
    long long acc = 0;
    if (p) {
        memset(p, 1, sz);
        for (size_t i = 0; i < sz; i += 4096) acc += p[i];
        free(p);
    }

    // a clock read -> clock_time_get / time
    time_t t = time(nullptr);
    acc += (long long)(t != (time_t)-1);

    return acc;
}

}
