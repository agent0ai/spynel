package media

/*
#ifdef __linux__
#include <features.h>
#endif
#ifdef __GLIBC__
#include <malloc.h>
#endif

static void spynel_trim_native_memory(void) {
#ifdef __GLIBC__
    malloc_trim(0);
#endif
}
*/
import "C"

// Other allocators reclaim freed native memory according to their own policy.
func trimNativeMemory() { C.spynel_trim_native_memory() }
