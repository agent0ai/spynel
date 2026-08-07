// Package miniaudio provides the narrow decoding surface Spynel needs from
// miniaudio 0.11.25. Playback, capture, encoding, and engine APIs are disabled
// at compile time.
package miniaudio

/*
#cgo linux LDFLAGS: -lm -lpthread -ldl -Wl,-rpath,$ORIGIN/lib
#cgo darwin LDFLAGS: -Wl,-rpath,@loader_path/lib
#include <stdlib.h>
#include "decoder.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"unsafe"
)

type Decoder struct {
	value *C.ma_decoder
}

func Open(path string) (*Decoder, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	var result C.ma_result
	decoder := C.spynel_ma_decoder_open(cPath, &result)
	if decoder == nil {
		return nil, resultError("open audio", result)
	}
	return &Decoder{value: decoder}, nil
}

func (d *Decoder) ReadPCMFrames(output []float32) (uint64, error) {
	if d == nil || d.value == nil {
		return 0, errors.New("miniaudio decoder is closed")
	}
	if len(output) == 0 {
		return 0, nil
	}
	var framesRead C.ma_uint64
	result := C.spynel_ma_decoder_read(
		d.value,
		(*C.float)(unsafe.Pointer(&output[0])),
		C.ma_uint64(len(output)),
		&framesRead,
	)
	runtime.KeepAlive(output)
	if result == C.MA_AT_END {
		return uint64(framesRead), io.EOF
	}
	if result != C.MA_SUCCESS {
		return uint64(framesRead), resultError("read audio", result)
	}
	return uint64(framesRead), nil
}

func (d *Decoder) Close() {
	if d != nil && d.value != nil {
		C.spynel_ma_decoder_close(d.value)
		d.value = nil
	}
}

func resultError(operation string, result C.ma_result) error {
	description := C.GoString(C.spynel_ma_result_description(result))
	return fmt.Errorf("%s: %s (%d)", operation, description, int(result))
}
