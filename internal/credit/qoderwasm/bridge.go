package qoderwasm

import (
	"context"
	"errors"

	"github.com/tetratelabs/wazero/api"
)

// wasmBridge implements just the pinned module's wasm-bindgen ABI. Temporary
// arguments live only in this request's runtime; returned strings and Rust
// objects are explicitly released, and closing the runtime reclaims all memory.
type wasmBridge struct {
	ctx  context.Context
	mod  api.Module
	heap *objectHeap
}

func (b *wasmBridge) call(name string, args ...uint64) ([]uint64, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	fn := b.mod.ExportedFunction(name)
	if fn == nil {
		return nil, errors.New("qoderwasm: missing WASM export " + name)
	}
	result, err := fn.Call(b.ctx, args...)
	if err != nil {
		// WASM errors can contain input-derived text. Never expose that text.
		return nil, errors.New("qoderwasm: WASM call failed: " + name)
	}
	return result, nil
}

func (b *wasmBridge) release(name string, args ...uint64) {
	// Cancellation closes the module; runtime.Close remains the final cleanup.
	if !b.mod.IsClosed() {
		_, _ = b.call(name, args...)
	}
}

func (b *wasmBridge) input(s string) ([]uint64, error) {
	r, err := b.call("__wbindgen_export2", uint64(len(s)), 1)
	if err != nil {
		return nil, err
	}
	ptr := uint32(r[0])
	if !b.mod.Memory().WriteString(ptr, s) {
		return nil, errors.New("qoderwasm: input memory out of bounds")
	}
	return []uint64{uint64(ptr), uint64(len(s))}, nil
}

func (b *wasmBridge) invokeFrame(name string, prefix []uint64, inputs []string) (uint32, func(), error) {
	r, err := b.call("__wbindgen_add_to_stack_pointer", uint64(uint32(0xfffffff0)))
	if err != nil {
		return 0, func() {}, err
	}
	sp := uint32(r[0])
	cleanup := func() { b.release("__wbindgen_add_to_stack_pointer", 16) }
	args := append([]uint64{uint64(sp)}, prefix...)
	for _, input := range inputs {
		pair, e := b.input(input)
		if e != nil {
			return 0, cleanup, e
		}
		args = append(args, pair...)
	}
	_, err = b.call(name, args...)
	return sp, cleanup, err
}

func (b *wasmBridge) words(sp uint32, count int) ([]uint32, error) {
	out := make([]uint32, count)
	for i := range out {
		v, ok := b.mod.Memory().ReadUint32Le(sp + uint32(i*4))
		if !ok {
			return nil, errors.New("qoderwasm: result memory out of bounds")
		}
		out[i] = v
	}
	return out, nil
}

func (b *wasmBridge) stringResult(name string, prefix []uint64, inputs []string, fallible bool) (string, error) {
	sp, cleanup, err := b.invokeFrame(name, prefix, inputs)
	defer cleanup()
	if err != nil {
		return "", err
	}
	count := 2
	if fallible {
		count = 4
	}
	words, err := b.words(sp, count)
	if err != nil {
		return "", err
	}
	if fallible && words[3] != 0 {
		b.heap.drop(words[2])
		return "", errors.New("qoderwasm: official module rejected " + name)
	}
	ptr, length := words[0], words[1]
	defer b.release("__wbindgen_export4", uint64(ptr), uint64(length), 1)
	buf, ok := b.mod.Memory().Read(ptr, length)
	if !ok {
		return "", errors.New("qoderwasm: string memory out of bounds")
	}
	return string(buf), nil
}

func (b *wasmBridge) pointerResult(name string, prefix []uint64, inputs []string) (uint32, error) {
	sp, cleanup, err := b.invokeFrame(name, prefix, inputs)
	defer cleanup()
	if err != nil {
		return 0, err
	}
	words, err := b.words(sp, 3)
	if err != nil {
		return 0, err
	}
	if words[2] != 0 {
		b.heap.drop(words[1])
		return 0, errors.New("qoderwasm: official module rejected " + name)
	}
	if words[0] == 0 {
		return 0, errors.New("qoderwasm: official module returned a null object")
	}
	return words[0], nil
}
