// Stand-in for WebRTC's checks.h: plain asserts.
#ifndef WEBRTC_BASE_CHECKS_H_
#define WEBRTC_BASE_CHECKS_H_

#include <cassert>

#define RTC_CHECK(c) assert(c)
#define RTC_CHECK_EQ(a, b) assert((a) == (b))
#define RTC_CHECK_NE(a, b) assert((a) != (b))
#define RTC_CHECK_GE(a, b) assert((a) >= (b))
#define RTC_CHECK_GT(a, b) assert((a) > (b))
#define RTC_CHECK_LE(a, b) assert((a) <= (b))
#define RTC_CHECK_LT(a, b) assert((a) < (b))
#define RTC_DCHECK(c) assert(c)
#define RTC_DCHECK_EQ(a, b) assert((a) == (b))
#define RTC_DCHECK_GE(a, b) assert((a) >= (b))
#define RTC_DCHECK_GT(a, b) assert((a) > (b))
#define RTC_DCHECK_LE(a, b) assert((a) <= (b))
#define RTC_DCHECK_LT(a, b) assert((a) < (b))

namespace rtc {
template <typename T>
inline T CheckedDivExact(T a, T b) {
  assert(a % b == 0);
  return a / b;
}
}  // namespace rtc

#endif
