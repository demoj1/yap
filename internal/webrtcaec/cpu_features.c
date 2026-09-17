// Stand-in for WebRTC's cpu_features: SSE2/SSE3 on x86 via the compiler,
// the plain C paths everywhere else.
#include "webrtc/system_wrappers/include/cpu_features_wrapper.h"

static int GetCPUInfo(CPUFeature feature) {
#if defined(__x86_64__) || defined(__i386__)
  if (feature == kSSE2) return __builtin_cpu_supports("sse2");
  if (feature == kSSE3) return __builtin_cpu_supports("sse3");
#else
  (void)feature;
#endif
  return 0;
}

static int GetCPUInfoNoASM(CPUFeature feature) {
  (void)feature;
  return 0;
}

WebRtc_CPUInfo WebRtc_GetCPUInfo = GetCPUInfo;
WebRtc_CPUInfo WebRtc_GetCPUInfoNoASM = GetCPUInfoNoASM;

uint64_t WebRtc_GetCPUFeaturesARM(void) { return 0; }
