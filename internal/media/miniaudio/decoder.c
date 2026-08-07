#define MINIAUDIO_IMPLEMENTATION
#include "decoder.h"

#include <stdlib.h>

ma_decoder* spynel_ma_decoder_open(const char* path, ma_result* result) {
    ma_decoder* decoder;
    ma_decoder_config config;

    if (result == NULL) {
        return NULL;
    }
    decoder = (ma_decoder*)malloc(sizeof(*decoder));
    if (decoder == NULL) {
        *result = MA_OUT_OF_MEMORY;
        return NULL;
    }
    config = ma_decoder_config_init(ma_format_f32, 1, 16000);
    *result = ma_decoder_init_file(path, &config, decoder);
    if (*result != MA_SUCCESS) {
        free(decoder);
        return NULL;
    }
    return decoder;
}

ma_result spynel_ma_decoder_read(ma_decoder* decoder, float* output, ma_uint64 frame_count, ma_uint64* frames_read) {
    return ma_decoder_read_pcm_frames(decoder, output, frame_count, frames_read);
}

void spynel_ma_decoder_close(ma_decoder* decoder) {
    if (decoder != NULL) {
        ma_decoder_uninit(decoder);
        free(decoder);
    }
}

const char* spynel_ma_result_description(ma_result result) {
    return ma_result_description(result);
}
