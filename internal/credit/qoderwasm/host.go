package qoderwasm

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const importModule = "./qoder_auth_wasm_bg.js"
const reservedObjects = 1028

type jsUndefined struct{}
type jsObject string
type jsFunction string

// A view keeps offsets rather than a Go slice, because WASM memory can grow.
type byteView struct{ ptr, length uint32 }

type objectHeap struct {
	objects []any
	free    []uint32
}

func newObjectHeap() *objectHeap {
	h := &objectHeap{objects: make([]any, reservedObjects)}
	for i := range h.objects {
		h.objects[i] = jsUndefined{}
	}
	h.objects[1025], h.objects[1026], h.objects[1027] = nil, true, false
	return h
}

func (h *objectHeap) add(v any) uint64 {
	if n := len(h.free); n != 0 {
		i := h.free[n-1]
		h.free = h.free[:n-1]
		h.objects[i] = v
		return uint64(i)
	}
	h.objects = append(h.objects, v)
	return uint64(len(h.objects) - 1)
}

func (h *objectHeap) get(i uint32) any {
	if uint64(i) >= uint64(len(h.objects)) {
		panic("qoderwasm: object index out of bounds")
	}
	return h.objects[i]
}

func (h *objectHeap) drop(i uint32) {
	if i < reservedObjects {
		return
	}
	_ = h.get(i)
	h.objects[i] = jsUndefined{}
	h.free = append(h.free, i)
}

func (h *objectHeap) take(i uint32) any {
	v := h.get(i)
	h.drop(i)
	return v
}

func memoryBytes(m api.Module, ptr, length uint32) []byte {
	buf, ok := m.Memory().Read(ptr, length)
	if !ok {
		panic("qoderwasm: host memory out of bounds")
	}
	return buf
}

func arrayBytes(m api.Module, v any) []byte {
	switch a := v.(type) {
	case []byte:
		return a
	case byteView:
		return memoryBytes(m, a.ptr, a.length)
	default:
		panic("qoderwasm: expected Uint8Array")
	}
}

func randomBytes(buf []byte) {
	if _, err := rand.Read(buf); err != nil {
		panic("qoderwasm: secure randomness unavailable")
	}
}

func boolValue(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func (h *objectHeap) instantiate(ctx context.Context, rt wazero.Runtime, compiled wazero.CompiledModule) error {
	builder := rt.NewHostModuleBuilder(importModule)
	for _, def := range compiled.ImportedFunctions() {
		module, name, _ := def.Import()
		if module != importModule {
			return errors.New("unexpected WASM import module")
		}
		fn := h.function(name)
		if fn == nil {
			return errors.New("unsupported WASM import")
		}
		builder.NewFunctionBuilder().WithGoModuleFunction(fn, def.ParamTypes(), def.ResultTypes()).Export(name)
	}
	_, err := builder.Instantiate(ctx)
	return err
}

// These adapters were independently written against the binary's imports, not
// copied from SDK JavaScript. Unknown imports fail closed. No JS interpreter,
// Node process, WASI, filesystem, sockets or ambient credentials are exposed.
func (h *objectHeap) function(name string) api.GoModuleFunc {
	switch name {
	case "__wbindgen_object_drop_ref":
		return func(_ context.Context, _ api.Module, s []uint64) { h.drop(uint32(s[0])) }
	case "__wbindgen_object_clone_ref":
		return func(_ context.Context, _ api.Module, s []uint64) { s[0] = h.add(h.get(uint32(s[0]))) }
	case "__wbindgen_cast_0000000000000001":
		return func(_ context.Context, m api.Module, s []uint64) {
			v := byteView{uint32(s[0]), uint32(s[1])}
			_ = memoryBytes(m, v.ptr, v.length)
			s[0] = h.add(v)
		}
	case "__wbindgen_cast_0000000000000002":
		return func(_ context.Context, m api.Module, s []uint64) {
			s[0] = h.add(string(memoryBytes(m, uint32(s[0]), uint32(s[1]))))
		}
	case "__wbg_new_99cabae501c0a8a0":
		return func(_ context.Context, _ api.Module, s []uint64) { s[0] = h.add(make(map[string]string)) }
	case "__wbg_set_08463b1df38a7e29":
		return func(_ context.Context, _ api.Module, s []uint64) {
			// Verified with the real module: this is Map.set, returning the Map,
			// NOT Uint8Array.set (whose ABI returns no value).
			m := h.get(uint32(s[0])).(map[string]string)
			k := h.get(uint32(s[1])).(string)
			v := h.get(uint32(s[2])).(string)
			m[k] = v
			s[0] = h.add(m)
		}
	case "__wbg_getRandomValues_d49329ff89a07af1":
		return func(_ context.Context, m api.Module, s []uint64) {
			randomBytes(memoryBytes(m, uint32(s[0]), uint32(s[1])))
		}
	case "__wbg_getRandomValues_c44a50d8cfdaebeb", "__wbg_randomFillSync_6c25eac9869eb53c":
		return func(_ context.Context, m api.Module, s []uint64) {
			if h.get(uint32(s[0])) != jsObject("crypto") {
				panic("qoderwasm: invalid crypto receiver")
			}
			randomBytes(arrayBytes(m, h.get(uint32(s[1]))))
		}
	case "__wbg_crypto_38df2bab126b63dc":
		return func(_ context.Context, _ api.Module, s []uint64) {
			if h.get(uint32(s[0])) == jsObject("global") {
				s[0] = h.add(jsObject("crypto"))
			} else {
				s[0] = h.add(jsUndefined{})
			}
		}
	case "__wbg_process_44c7a14e11e9f69e", "__wbg_versions_276b2795b1c6a219", "__wbg_node_84ea875411254db1", "__wbg_msCrypto_bd5a034af96bcba6":
		// The host supplies Web Crypto, not a Node or legacy browser runtime.
		return func(_ context.Context, _ api.Module, s []uint64) { s[0] = h.add(jsUndefined{}) }
	case "__wbg_require_b4edbdcf3e2a1ef0":
		return func(_ context.Context, _ api.Module, s []uint64) { s[0] = h.add(jsFunction("require")) }
	case "__wbg_call_d578befcc3145dee":
		return func(_ context.Context, _ api.Module, s []uint64) {
			if h.get(uint32(s[0])) != jsFunction("require") || h.get(uint32(s[2])) != "crypto" {
				panic("qoderwasm: unsupported host function call")
			}
			s[0] = h.add(jsObject("crypto"))
		}
	case "__wbg_static_accessor_GLOBAL_THIS_a1248013d790bf5f", "__wbg_static_accessor_GLOBAL_f2e0f995a21329ff":
		return func(_ context.Context, _ api.Module, s []uint64) { s[0] = h.add(jsObject("global")) }
	case "__wbg_static_accessor_SELF_24f78b6d23f286ea", "__wbg_static_accessor_WINDOW_59fd959c540fe405":
		return func(_ context.Context, _ api.Module, s []uint64) { s[0] = 0 }
	case "__wbg_now_88621c9c9a4f3ffc":
		return func(_ context.Context, _ api.Module, s []uint64) {
			s[0] = math.Float64bits(float64(time.Now().UnixMilli()))
		}
	case "__wbg_new_with_length_9cedd08484b73942":
		return func(_ context.Context, _ api.Module, s []uint64) {
			n := uint32(s[0])
			if n > 16<<20 {
				panic("qoderwasm: Uint8Array size exceeds limit")
			}
			s[0] = h.add(make([]byte, n))
		}
	case "__wbg_prototypesetcall_3e05eb9545565046":
		return func(_ context.Context, m api.Module, s []uint64) {
			// The binary passes a destination memory slice (ptr, len), then
			// a source JS array handle, not (array, array, offset).
			dst := memoryBytes(m, uint32(s[0]), uint32(s[1]))
			src := arrayBytes(m, h.get(uint32(s[2])))
			if len(src) > len(dst) {
				panic("qoderwasm: Uint8Array.set out of bounds")
			}
			copy(dst, src)
		}
	case "__wbg_subarray_0f98d3fb634508ad":
		return func(_ context.Context, m api.Module, s []uint64) {
			v := h.get(uint32(s[0]))
			a := arrayBytes(m, v)
			start, end := subarrayIndex(int32(s[1]), len(a)), subarrayIndex(int32(s[2]), len(a))
			if end < start {
				end = start
			}
			if view, ok := v.(byteView); ok {
				s[0] = h.add(byteView{view.ptr + uint32(start), uint32(end - start)})
			} else {
				s[0] = h.add(a[start:end])
			}
		}
	case "__wbg_length_0c32cb8543c8e4c8":
		return func(_ context.Context, m api.Module, s []uint64) {
			s[0] = uint64(len(arrayBytes(m, h.get(uint32(s[0])))))
		}
	case "__wbg_Error_2e59b1b37a9a34c3":
		return func(_ context.Context, _ api.Module, s []uint64) { s[0] = h.add(errors.New("official WASM error")) }
	case "__wbg___wbindgen_throw_81fc77679af83bc6":
		return func(_ context.Context, _ api.Module, _ []uint64) {
			panic("qoderwasm: official WASM threw an exception")
		}
	case "__wbg___wbindgen_is_undefined_c0cca72b82b86f4d":
		return func(_ context.Context, _ api.Module, s []uint64) {
			_, ok := h.get(uint32(s[0])).(jsUndefined)
			s[0] = boolValue(ok)
		}
	case "__wbg___wbindgen_is_string_b29b5c5a8065ba1a":
		return func(_ context.Context, _ api.Module, s []uint64) {
			_, ok := h.get(uint32(s[0])).(string)
			s[0] = boolValue(ok)
		}
	case "__wbg___wbindgen_is_function_49868bde5eb1e745":
		return func(_ context.Context, _ api.Module, s []uint64) {
			_, ok := h.get(uint32(s[0])).(jsFunction)
			s[0] = boolValue(ok)
		}
	case "__wbg___wbindgen_is_object_40c5a80572e8f9d3":
		return func(_ context.Context, _ api.Module, s []uint64) {
			object := false
			switch h.get(uint32(s[0])).(type) {
			case jsObject, map[string]string, []byte, byteView, error:
				object = true
			}
			s[0] = boolValue(object)
		}
	default:
		return nil
	}
}

func subarrayIndex(i int32, length int) int {
	n := int64(i)
	if n < 0 {
		n += int64(length)
	}
	if n < 0 {
		return 0
	}
	if n > int64(length) {
		return length
	}
	return int(n)
}
