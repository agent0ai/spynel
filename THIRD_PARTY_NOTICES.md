# Third-party notices

Spynel includes open-source Go modules recorded exactly in `go.mod` and `go.sum`. Their source and license files are available from the module repositories identified by `go list -m all`.

Notable runtime components include:

- [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) and its official Go bindings, Apache License 2.0. Release archives include its native C API runtime.
- [ONNX Runtime](https://github.com/microsoft/onnxruntime), MIT License, distributed as part of the sherpa-onnx native runtime.
- [miniaudio 0.11.25](https://github.com/mackron/miniaudio), MIT No Attribution. Spynel includes the unmodified upstream header and a narrow local decoding adapter.
- [Pion Opus](https://github.com/pion/opus), MIT License. Spynel uses its pure-Go RFC 6716 decoder for Ogg/Opus voice messages; the full license is included in release archives.
- [NVIDIA Parakeet Unified EN 0.6B](https://huggingface.co/nvidia/parakeet-unified-en-0.6b), governed by the NVIDIA Open Model License Agreement. Spynel downloads the k2-fsa INT8 ONNX conversion on demand rather than redistributing the weights.
- [NVIDIA Parakeet TDT 0.6B v3](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3), © NVIDIA Corporation and licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). Spynel downloads k2-fsa's converted and INT8-quantized ONNX form on demand; those format and quantization changes are not made by Spynel.
- [whatsmeow](https://github.com/tulir/whatsmeow), Mozilla Public License 2.0. Spynel uses the unmodified upstream module to implement WhatsApp multi-device support. The corresponding source is available from that repository and the exact pseudo-version is recorded in `go.mod`.
- Bubble Tea, Bubbles, and Lip Gloss, MIT License.
- ncruces/go-sqlite3, MIT License.
- qrterminal, MIT License.
- google.golang.org/protobuf, BSD 3-Clause License.
- gopkg.in/yaml.v3, Apache License 2.0 and MIT License.

All third-party components remain subject to their respective copyright and license terms. This notice does not replace those licenses.
