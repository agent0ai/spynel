// Narrow workaround for sherpa-onnx's pinned ONNX Runtime 1.27.0 on virtual
// Linux ARM64 CPUs. CPUIDInfo::LogEarlyWarning writes three chunks to std::cerr
// during shared-library initialization, before a runtime logger can exist.
// ELF symbol interposition reaches that boundary; a Go init/main logger cannot.
// Do not change CPU feature detection, the module-cache libraries or stderr fd.
// ponytail: pinned three-chunk diagnostic; remove when the native library stops
// emitting it. Changed diagnostics pass through instead of being guessed at.
#include <stdio.h>
#include <stdatomic.h>
#include <string.h>

static const char prefix[] = "onnxruntime cpuid_info warning: ";
static const char message[] = "Unknown CPU vendor. cpuinfo_vendor value: 0";
static atomic_int startup = 1;
static _Thread_local int pending;

static int flush_pending(FILE *stream) {
  int ok = 1;
  if (pending >= 1)
    ok = fwrite_unlocked(prefix, 1, sizeof(prefix)-1, stream) == sizeof(prefix)-1;
  if (pending == 2)
    ok = (fwrite_unlocked(message, 1, sizeof(message)-1, stream) == sizeof(message)-1) && ok;
  pending = 0;
  return ok;
}

size_t fwrite(const void *data, size_t size, size_t count, FILE *stream) {
  flockfile(stream);
  if (startup && stream == stderr && size == 1) {
    if (pending == 1 && count == sizeof(message)-1 && memcmp(data, message, count) == 0) {
      pending = 2;
      funlockfile(stream);
      return count;
    }
    if (pending == 2 && count == 1 && *(const char *)data == '\n') {
      pending = 0;
      funlockfile(stream);
      return count;
    }
    if (!flush_pending(stream)) {
      funlockfile(stream);
      return 0;
    }
    if (count == sizeof(prefix)-1 && memcmp(data, prefix, count) == 0) {
      pending = 1;
      funlockfile(stream);
      return count;
    }
  } else if (stream == stderr && !flush_pending(stream)) {
    funlockfile(stream);
    return 0;
  }
  size_t result = fwrite_unlocked(data, size, count, stream);
  funlockfile(stream);
  return result;
}

// Executable constructors run after dependency constructors and before main.
// Preserve an incomplete/unrecognized line and permanently stop filtering.
__attribute__((constructor)) static void finish_native_startup(void) {
  flockfile(stderr);
  flush_pending(stderr);
  startup = 0;
  funlockfile(stderr);
}
