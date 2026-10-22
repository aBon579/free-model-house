package lane

import "testing"

// TestClassifyUnsupportedInput 验证"模型不支持这种输入"的分类。
//
// ⚠️ 上游原话（实测 2026-10-20）："Model does not support this protocol."
//
// ⚠️ **它不能被当成"不可用"** ——
//
//	那个模型**本身是好的**（给它一张图就能用），
//	只是这次请求的类型它接不了。
//	标成"不可用"会让它从清单里消失，用户再也看不到它。
func TestClassifyUnsupportedInput(t *testing.T) {
	e := ClassifyFailure(0, `{"error":{"message":"Model does not support this protocol."}}`, 0)
	if e.Code != CodeUnsupportedInput {
		t.Fatalf("该分类为 UNSUPPORTED_INPUT，实际 %q", e.Code)
	}
	// ⚠️ 换模型**有用**（换个纯文本模型就行）。
	if !e.Switchable() {
		t.Fatal("换模型应该有用")
	}
	// ⚠️ 但重放**同样的** body 还是会撞同一个拒绝。
	if e.RetrySameModel() {
		t.Fatal("不该重试同一个模型")
	}
	// ⚠️ scope 是 Provider（模型级）—— 不是 Request。
	if e.ScopeOf() != ScopeProvider {
		t.Fatalf("scope 该是 provider，实际 %q", e.ScopeOf())
	}
}

// TestProbeStateUnsupportedInput 验证探测时它**保持"未探测"**。
//
// ⚠️ 不能是"不可用" —— 那样模型会从清单里消失。
func TestProbeStateUnsupportedInput(t *testing.T) {
	e := &Error{Code: CodeUnsupportedInput, Message: "Model does not support this protocol."}
	state, detail := probeStateFromError(e)
	if state == StateUnavailable {
		t.Fatal("不该判为不可用 —— 模型本身是好的")
	}
	if state != StateUnknown {
		t.Fatalf("该保持 unknown，实际 %q", state)
	}
	if detail == "" {
		t.Fatal("该带上说明")
	}
}

// TestClassifyStillCatchesMissingModel 验证**没破坏**原有的分类。
func TestClassifyStillCatchesMissingModel(t *testing.T) {
	e := ClassifyFailure(404, `{"error":{"message":"model xyz not found"}}`, 0)
	if e.Code == CodeUnsupportedInput {
		t.Fatal("不存在的模型不该归为「输入不支持」")
	}
	if !e.Unavailable {
		t.Fatal("不存在的模型该是 Unavailable")
	}
}
