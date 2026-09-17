// A C face for the WebRTC (legacy) echo canceller at 48 kHz: the signal is
// split into three 16 kHz bands, the canceller works on the lowest and
// suppresses the rest, and the bands are put back together.
#include <stdint.h>
#include <string.h>

extern "C" {
#include "webrtc/modules/audio_processing/aec/aec_core.h"
#include "webrtc/modules/audio_processing/aec/include/echo_cancellation.h"
}
#include "webrtc/modules/audio_processing/three_band_filter_bank.h"

namespace {
const int kRate = 48000;
const int kFrame = 480;  // 10 ms
const int kBands = 3;
const int kBand = kFrame / kBands;
}  // namespace

extern "C" {

struct yap_aec {
  void* aec;
  webrtc::ThreeBandFilterBank* far_bank;
  webrtc::ThreeBandFilterBank* near_bank;
  float far_buf[kBands][kBand];
  float in_buf[kBands][kBand];
  float out_buf[kBands][kBand];
};

// yap_aec_reset starts adapting from scratch (the devices changed) with
// nlp: 0 conservative, 1 moderate, 2 aggressive residual suppression.
void yap_aec_reset(yap_aec* h, int nlp) {
  WebRtcAec_Init(h->aec, kRate, kRate);
  AecConfig cfg;
  cfg.nlpMode = nlp;
  cfg.skewMode = kAecFalse;
  cfg.metricsMode = kAecFalse;
  cfg.delay_logging = kAecTrue;
  WebRtcAec_set_config(h->aec, cfg);
  // What Chrome shipped: the canceller finds the delay itself and the
  // longer filter rides out a delay that moves.
  WebRtcAec_enable_delay_agnostic(WebRtcAec_aec_core(h->aec), 1);
  WebRtcAec_enable_extended_filter(WebRtcAec_aec_core(h->aec), 1);
}

yap_aec* yap_aec_create(int nlp) {
  yap_aec* h = new yap_aec;
  memset(h->far_buf, 0, sizeof(h->far_buf));
  h->aec = WebRtcAec_Create();
  yap_aec_reset(h, nlp);
  h->far_bank = new webrtc::ThreeBandFilterBank(kFrame);
  h->near_bank = new webrtc::ThreeBandFilterBank(kFrame);
  return h;
}

void yap_aec_free(yap_aec* h) {
  WebRtcAec_Free(h->aec);
  delete h->far_bank;
  delete h->near_bank;
  delete h;
}

// yap_aec_status is 1 while the canceller hears the speakers in the mic.
int yap_aec_status(yap_aec* h) {
  int status = 0;
  WebRtcAec_get_echo_status(h->aec, &status);
  return status;
}

// yap_aec_far hands over the 10 ms the speakers are about to play.
void yap_aec_far(yap_aec* h, const int16_t* pcm) {
  float in[kFrame];
  for (int i = 0; i < kFrame; i++) in[i] = pcm[i];
  float* bands[kBands] = {h->far_buf[0], h->far_buf[1], h->far_buf[2]};
  h->far_bank->Analysis(in, kFrame, bands);
  WebRtcAec_BufferFarend(h->aec, h->far_buf[0], kBand);
}

// yap_aec_process takes the echo out of 10 ms of microphone, in place.
// delay_ms is the caller's guess of speakers-to-mic latency; the
// canceller refines it. Returns 0 on success.
int yap_aec_process(yap_aec* h, int16_t* pcm, int delay_ms) {
  float in[kFrame];
  for (int i = 0; i < kFrame; i++) in[i] = pcm[i];
  float* near_bands[kBands] = {h->in_buf[0], h->in_buf[1], h->in_buf[2]};
  float* out_bands[kBands] = {h->out_buf[0], h->out_buf[1], h->out_buf[2]};
  h->near_bank->Analysis(in, kFrame, near_bands);
  int rc = WebRtcAec_Process(h->aec, near_bands, kBands, out_bands, kBand, (int16_t)delay_ms, 0);
  if (rc != 0) return rc;
  // Headphones: the canceller hears no echo, so the suppressor would only
  // dent the voice while the other side talks. Keep the mic as it was.
  if (!yap_aec_status(h)) return 0;
  float out[kFrame];
  h->near_bank->Synthesis(out_bands, kBand, out);
  for (int i = 0; i < kFrame; i++) {
    float v = out[i];
    pcm[i] = v > 32767 ? 32767 : v < -32768 ? -32768 : (int16_t)v;
  }
  return 0;
}

// yap_aec_delay reports the delay the canceller settled on, in ms of the
// 16 kHz band, or -1 while it has none.
int yap_aec_delay(yap_aec* h) {
  int median, std;
  float fraction_poor;
  if (WebRtcAec_GetDelayMetrics(h->aec, &median, &std, &fraction_poor) != 0) return -1;
  return median;
}

}  // extern "C"
