package qoderwasm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestAuthFieldsABI(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(compilationCache).WithCloseOnContextDone(true).WithMemoryLimitPages(4096))
	defer rt.Close(ctx)
	compiled, err := rt.CompileModule(ctx, officialWASM)
	if err != nil {
		t.Fatal(err)
	}
	h := newObjectHeap()
	if err := h.instantiate(ctx, rt, compiled); err != nil {
		t.Fatal(err)
	}
	m, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithStartFunctions())
	if err != nil {
		t.Fatal(err)
	}
	b := wasmBridge{ctx: ctx, mod: m, heap: h}
	frame, err := b.call("__wbindgen_add_to_stack_pointer", 0xfffffff0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.release("__wbindgen_add_to_stack_pointer", 16)
	input, err := b.input(string(fixtureUser()))
	if err != nil {
		t.Fatal(err)
	}
	// Raw errors are useful here: this test supplies only synthetic credentials.
	_, err = m.ExportedFunction("generate_runtime_auth_fields").Call(ctx, frame[0], input[0], input[1])
	if err != nil {
		t.Fatal(err)
	}
	words, err := b.words(uint32(frame[0]), 4)
	if err != nil || words[3] != 0 {
		t.Fatalf("authentication ABI failed: words=%v, error=%v", words, err)
	}
	defer b.release("__wbindgen_export4", uint64(words[0]), uint64(words[1]), 1)
	var fields authFields
	if err := json.Unmarshal(memoryBytes(m, words[0], words[1]), &fields); err != nil {
		t.Fatal(err)
	}
	if fields.EncryptUserInfo == "" || fields.Key == "" {
		t.Fatal("authentication fields missing")
	}
	contextJSON, _ := json.Marshal(map[string]any{"uid": fixtureUID, "encrypt_user_info": fields.EncryptUserInfo, "key": fields.Key, "organization_id": "", "organization_tags": []string{}, "data_policy_agreed": false})
	contextPtr, err := b.pointerResult("qodercontext_new", nil, []string{"fixture-machine", "0.1.0", string(contextJSON), `{"client_type":"5","business_product":"cli","business_type":"agent","scene":"assistant"}`})
	if err != nil {
		t.Fatal(err)
	}
	defer b.release("__wbg_qodercontext_free", uint64(contextPtr), 0)
	resultPtr, err := b.pointerResult("qodercontext_prepareInferRequest", []uint64{uint64(contextPtr)}, []string{"https://fixture.example", fixturePayload, "fixture-model", "system"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.release("__wbg_requestresult_free", uint64(resultPtr), 0)
	headers, err := b.call("requestresult_headers", uint64(resultPtr))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture headers: %v", h.take(uint32(headers[0])))
	body, err := b.stringResult("requestresult_body", []uint64{uint64(resultPtr)}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture body: %s", body)
}
