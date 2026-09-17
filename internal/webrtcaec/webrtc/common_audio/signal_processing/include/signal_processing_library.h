// Stand-in for WebRTC's signal processing library: the AEC only draws
// random numbers from it (comfort noise).
#ifndef WEBRTC_SPL_SIGNAL_PROCESSING_LIBRARY_H_
#define WEBRTC_SPL_SIGNAL_PROCESSING_LIBRARY_H_

#include "webrtc/typedefs.h"

#define WEBRTC_SPL_WORD16_MAX 32767
#define WEBRTC_SPL_WORD16_MIN -32768
#define WEBRTC_SPL_MIN(A, B) ((A) < (B) ? (A) : (B))
#define WEBRTC_SPL_MAX(A, B) ((A) > (B) ? (A) : (B))
#define WEBRTC_SPL_SAT(a, b, c) ((b) > (a) ? (a) : (b) < (c) ? (c) : (b))

#ifdef __cplusplus
extern "C" {
#endif

int16_t WebRtcSpl_RandU(uint32_t* seed);
int16_t WebRtcSpl_RandN(uint32_t* seed);
int16_t WebRtcSpl_RandUArray(int16_t* vector, int16_t vector_length, uint32_t* seed);

#ifdef __cplusplus
}
#endif

#endif
