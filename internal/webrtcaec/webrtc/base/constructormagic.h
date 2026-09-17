#ifndef WEBRTC_BASE_CONSTRUCTORMAGIC_H_
#define WEBRTC_BASE_CONSTRUCTORMAGIC_H_

#define RTC_DISALLOW_COPY_AND_ASSIGN(TypeName) \
  TypeName(const TypeName&) = delete;           \
  void operator=(const TypeName&) = delete

#endif
