#ifndef SPYNEL_MINIAUDIO_DECODER_H
#define SPYNEL_MINIAUDIO_DECODER_H

#define MA_NO_DEVICE_IO
#define MA_NO_ENGINE
#define MA_NO_RESOURCE_MANAGER
#define MA_NO_ENCODING
#define MA_NO_GENERATION

#include "miniaudio.h"

ma_decoder* spynel_ma_decoder_open(const char* path, ma_result* result);
ma_result spynel_ma_decoder_read(ma_decoder* decoder, float* output, ma_uint64 frame_count, ma_uint64* frames_read);
void spynel_ma_decoder_close(ma_decoder* decoder);
const char* spynel_ma_result_description(ma_result result);

#endif
