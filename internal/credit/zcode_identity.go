package credit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// **ZCode 官方客户端身份块**的拼装与准入逻辑。
//
// 上游对 ZCode 的推理通道做**请求体内容检查**：system 里缺少官方身份块结构时，
// 直接回 405 + `{"code":3012,"msg":"request has been blocked due to unusual activity."}`。
// 参考项目的实测矩阵（同账号、同 captcha 来源，2026-10-03 复测）：
//
//	无 system                            → ✗ 405 + 3012
//	仅 cliPrefix（42 字符）               → ✗ 405 + 3012
//	cliPrefix + stable（三段，合计 2900） → ✓ 200
//	再加 dynamic 段（合计 7599）           → ✓ 200
//
// ⇒ 判据是「**身份块在不在**」，与块数、HTTP 头、运行时（Electron / curl / Node）都无关。
// ⇒ 日期块**不是**判据（去掉照样 200），照发只因为官方如此。
//
// ⚠️ 这段的作用是**让上游把我们当成官方客户端**，不是"把请求修得更正确"。所以它：
//
//	按账号开启、默认关闭（Account.OfficialIdentity），关掉即恢复成 FMH 自述块。
//	上游对 3012 有阶梯处罚：一次 30 分钟，24h 内第 3 次起 24 小时，累计 5 次停用账号。
//	开启后**仍然**收到 3012，说明模板已经和上游当前策略脱钩 —— 那时应当立刻关掉，
//	不要拿自己的账号反复试。条款层面的后果由开启的人承担。
//
// 文本常量在 zcode_identity_text.go（程序化提取，勿手改）。

// zcodeIdentityText 拼出准入判定的那段身份文本。
func zcodeIdentityText() string {
	return zcodeOfficialCLIPrefix + "\n\n" + strings.Join(zcodeOfficialStableSections, "\n\n")
}

// zcodeIdentitySane 报告身份模板是否还是提取时的样子。
//
// ⚠️ 为什么要在**发送前**卡：模板被改动后的请求不是"退回裸请求"，而是
//
//	**多烧一次账号处罚**（30 分钟 → 24h → 停用），而且失败原因看起来像
//	"这个账号被风控了"，排查方向会被整个带偏。宁可本地拒发。
func zcodeIdentitySane() bool {
	return len([]rune(zcodeIdentityText())) == zcodeOfficialIdentityChars
}

// zcodeEnv 是官方 # Environment 段里那些**进程内不变**的量。
type zcodeEnv struct {
	cwd      string
	git      bool
	platform string
	goarch   string
	shell    string
}

var zcodeEnvOnce sync.Once
var zcodeEnvValue zcodeEnv

func zcodeRuntimeEnv() zcodeEnv {
	zcodeEnvOnce.Do(func() {
		cwd, err := os.Getwd()
		if err != nil || strings.TrimSpace(cwd) == "" {
			cwd = "."
		}
		platform := runtime.GOOS
		shell := "bash"
		if platform == "windows" {
			shell = "powershell"
		}
		_, gitErr := os.Stat(filepath.Join(cwd, ".git"))
		zcodeEnvValue = zcodeEnv{
			cwd:      cwd,
			git:      gitErr == nil,
			platform: platform,
			goarch:   runtime.GOARCH,
			shell:    shell,
		}
	})
	return zcodeEnvValue
}

// zcodeEnvironmentSection 是官方形态的 # Environment 段。
//
// 不发的实测后果：模型答"你是谁"只能给个笼统的厂商名，也不知道自己在哪个平台、
// 哪个工作目录 —— 于是会用相对路径瞎猜、该给 PowerShell 的时候给 bash。
// 段内只有模型名逐请求变，其余按进程缓存。
func zcodeEnvironmentSection(model string) string {
	e := zcodeRuntimeEnv()
	git := "no"
	if e.git {
		git = "yes"
	}
	return strings.Join([]string{
		"# Environment",
		"You have been invoked in the following environment:",
		" - Primary working directory: " + e.cwd,
		" - Is a git repository: " + git,
		" - Platform: " + e.platform,
		" - Shell: " + e.shell,
		" - OS Version: " + e.platform + " " + e.goarch,
		" - You are powered by the model named zcode/" + model + ".",
	}, "\n")
}

// zcodeOfficialSystem 把已经备好的 system 块重排成官方形态。
//
// 块序：cliPrefix → stable → environment → 调用方原有内容（含 FMH 的自述块）。
//
// ⚠️ 官方内容必须**在开头** —— 上游的检查看的是前缀结构；调用方的只能追加在最后。
//
// ⚠️ FMH 的自述块**保留**：开了身份模拟也不对最终用户隐瞒这是第三方客户端。
//
// ⚠️ cache_control 只打给最后一块 —— Anthropic 的 prompt caching 是前缀式的，
//
//	最后一个断点已经覆盖前面全部；逐块打点会吃满单请求 4 个的预算，tools 反而打不上。
//	它也不参与准入判定。
func zcodeOfficialSystem(caller []any, model string) ([]any, error) {
	if !zcodeIdentitySane() {
		return nil, zcodeError(409, "official_identity_stale", "ZCode 身份模板与提取时不一致，已拒绝发送 —— 继续发只会加重账号的风控处罚；请同步模板或关掉该账号的「官方客户端身份」")
	}
	blocks := []any{
		map[string]any{"type": "text", "text": zcodeOfficialCLIPrefix},
		map[string]any{"type": "text", "text": zcodeIdentityText()},
		map[string]any{"type": "text", "text": zcodeEnvironmentSection(model)},
	}
	blocks = append(blocks, caller...)
	if last, ok := blocks[len(blocks)-1].(map[string]any); ok {
		last["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return blocks, nil
}

const zcodeContextIntro = "As you answer the user's questions, you can use the following context:"

// ⚠️ 结尾段前有 6 个空格缩进，官方逐字如此。
const zcodeContextOutro = "      IMPORTANT: this context may or may not be relevant to your tasks. " +
	"You should not respond to this context unless it is highly relevant to your task."

// zcodeContextPrefixBlock 是 <system-reminder> 日期块（官方逐字形态）。
//
// ⚠️ 它**不是** 3012 的判据 —— "缺了它就一定 3012" 是早期窗口的说法，
// 已被参考项目用三个账号、17 次请求复测推翻。照发只因为官方如此、零成本。
func zcodeContextPrefixBlock(now time.Time) map[string]any {
	body := strings.Join([]string{
		zcodeContextIntro,
		"# currentDate\nToday's date is " + now.Format("2006-01-02") + ".",
		"",
		zcodeContextOutro,
	}, "\n")
	return map[string]any{"type": "text", "text": "<system-reminder>" + body + "</system-reminder>"}
}

// zcodeWithContextPrefix 给首轮 user 消息的 content 数组最前面插日期块。
//
// 官方行为：只处理第一条、必须是 user、**幂等**（已经以 system-reminder 开头就不再插）。
// 日期用**本地时区**，不是 UTC。
func zcodeWithContextPrefix(messages []map[string]any, now time.Time) []map[string]any {
	if len(messages) == 0 || messages[0]["role"] != "user" {
		return messages
	}
	blocks := Array(messages[0]["content"])
	if len(blocks) > 0 {
		if first, ok := blocks[0].(map[string]any); ok {
			if s, _ := first["text"].(string); strings.HasPrefix(s, "<system-reminder>") {
				return messages
			}
		}
	}
	out := append([]map[string]any(nil), messages...)
	clone := map[string]any{}
	for k, v := range out[0] {
		clone[k] = v
	}
	clone["content"] = append([]any{zcodeContextPrefixBlock(now)}, blocks...)
	out[0] = clone
	return out
}
