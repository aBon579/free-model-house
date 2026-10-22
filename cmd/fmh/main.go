// Free Model House —— 一个跑在 NAS 上的免费模型聚合网关。
// 它把 OpenCode Zen 的免密免费车道与 OpenRouter 的免费模型
// 聚合成一个**局域网内**的 OpenAI / Anthropic 兼容接口，
// 让 LAN 里的 agent 用一把 `fmh-` 开头的 Key 直接用。
// 用法：
//
//	fmh                     正常启动
//	fmh -addr :8678         换端口
//	fmh -data /data         换数据目录
//	fmh healthcheck         健康检查（容器用）
//	fmh diag                运行环境诊断（CA 证书 / 时区 / HTTPS 连通性）
//	fmh version             打印版本
//
// ⚠️ 详见 docs/design/00-overview.md。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/server"
	"github.com/aBon579/free-model-house/internal/store"
)

func main() {
	var (
		addr    = flag.String("addr", envOr("FMH_ADDR", ":8678"), "监听地址")
		dataDir = flag.String("data", envOr("FMH_DATA", "/data"), "数据目录")
	)
	flag.Parse()

	// 子命令。
	switch flag.Arg(0) {
	case "healthcheck":
		os.Exit(runHealthcheck(*addr))
	case "diag":
		os.Exit(runDiag())
	case "version":
		fmt.Println("free-model-house " + server.Version)
		return
	case "key":
		os.Exit(runShowKey(*dataDir, flag.Arg(1)))
	}

	// ⚠️ **日志同时写文件** ——
	//
	//   stdout 只进 docker logs，容器一重建就没了。
	//   而排障最需要的恰恰是"**当时发生了什么**"。
	//
	//   写到 <数据目录>/fmh.log —— 那是 bind mount，容器外也读得到。
	//   降权**之后**才能写（那时数据目录已经归 nobody 了），
	//   所以文件在 run() 里开，不在这里。
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)
	logger.SetPrefix("fmh ")

	if err := run(*addr, *dataDir, logger); err != nil {
		logger.Printf("致命错误: %v", err)
		os.Exit(1)
	}
}

// detailSuffix 把探测结论的原因拼成日志后缀。
func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return "：" + detail
}

// run 是主流程。
func run(addr, dataDir string, logger *log.Logger) error {
	// ⚠️ **CA 证书必须在降权之前读进内存** ——
	//
	//   降权后进程是 nobody(65534)。
	//   如果 `/etc/ssl/certs/ca-certificates.crt` 对 nobody 不可读
	//   （上传权限是 0600 时就会这样），
	//   Go 就没有证书池可用 → **所有 HTTPS 请求都失败**：
	//
	//     open /etc/ssl/certs/ca-certificates.crt: permission denied
	//
	//   ⚠️ **症状非常好认** ——
	//   请求在 TLS 阶段就失败，**不等网络**，
	//   所以探测"快得离谱"（27 个模型几秒就完）。
	//   正常情况每个模型要几秒到几十秒。
	//
	//   所以这里**必须排在降权前面** —— 趁还是 root 时读出来。
	if _, ok, err := lane.LoadCACerts(); ok {
		logger.Printf("CA 证书已加载（%d 张）", lane.CACertCount())
	} else if err != nil {
		logger.Printf("⚠️ CA 证书加载失败（将用系统默认）: %v", err)
	}

	// ⚠️ **降权必须在打开数据库之前** —— 降权之后进程就**不能再 chown** 了，
	// 而数据目录正是那时候才被交到非特权用户手里的。
	// 这解决的是 Docker 的死结：bind mount 的目录属主是 root:root 0755，
	// 而容器进程要是非特权用户，就**写不进去**。
	if err := dropPrivilegesIfRoot(dataDir); err != nil {
		return fmt.Errorf("准备数据目录权限失败: %w", err)
	}

	// ⚠️ **开日志文件** ——
	//
	//   写到 <数据目录>/fmh.log（bind mount，容器外能直接拿）。
	//   排障最需要的是"当时发生了什么"，而 docker logs 会随容器重建消失。
	//
	//   ⚠️ **必须在降权之后开** —— 降权前数据目录还不是 nobody 的，
	//   降权后写进去的文件属主才正确（nobody 能继续追加）。
	attachLogFile(logger, dataDir)

	db, err := store.Open(dataDir)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.Close()
	logger.Printf("数据库 %s", db.Path())

	// 密封存储（上游凭据）。
	vault, err := store.NewVault(dataDir)
	if err != nil {
		return fmt.Errorf("初始化凭据存储失败: %w", err)
	}
	if !vault.HasKey() {
		return fmt.Errorf("没有可用的凭据加密密钥（检查 FMH_SECRET_KEY 或 %s）", dataDir)
	}

	keys := server.NewKeys(db)
	// ⚠️ **默认打印完整 Key** —— 不是指纹。
	//   为什么：启动日志是**唯一**可靠的取 Key 途径。
	//   管理台要登录才能看，而登录要 Key —— 鸡生蛋。
	//   `docker exec free-model-house /fmh key` 也行，但容器起不来时用不了。
	//   想要更保守的行为：设 `FMH_HIDE_KEYS=1`。
	logger.Printf("局域网 Key %s", showKey(keys.LanKey()))

	// ⚠️ **管理台密码** ——
	//   初始密码是 `admin`，第一次登录后**强制**改。
	password := server.NewPassword(db)
	if password.IsDefault() {
		logger.Printf("管理台初始密码：%s（首次登录后必须修改）", server.DefaultAdminPassword)
	}

	registry := lane.NewRegistry()
	// ⚠️ 会话映射要**持久化** —— 重启后复用同一个上游 session，
	// prompt cache 才不会每次冷启动。
	sessions := lane.NewSessionMap()
	sessions.SetPersist(func(seed, id string) {
		if err := db.PutSession(seed, id); err != nil {
			logger.Printf("存会话映射失败: %v", err)
		}
	})
	if saved, err := db.LoadSessions(); err == nil {
		sessions.Seed(saved)
	}
	zen := lane.NewZenLane(sessions)
	registry.Register(zen)
	orLane := lane.NewOpenRouterLane("")
	registry.Register(orLane)
	// ⚠️ **Kilo 免 key 车道** ——
	//
	// 它是**最省事**的一个源：标准 OpenAI 兼容，
	// 免费切片**完全不需要凭据**（不要 Key、不要登录、没有指纹闸门）。
	//
	// 而且它和 OpenRouter 的模型 id **重叠** ——
	// 同样的模型走 Kilo **不花用户的 OR 额度**。
	kilo := lane.NewKiloLane()
	registry.Register(kilo)

	catalog := lane.NewCatalog()
	// ⚠️ 限流账本要**和路由共享同一个实例** —— 否则恢复进去的记录路由器根本看不到。
	book := lane.NewBook()
	router := lane.NewRouter(catalog, registry, book)

	// ⚠️ **车道开关** —— 哪条车道开不开。
	//
	// 关掉的车道的模型仍然显示在清单里，但**不进候选链**。
	switches := lane.NewLaneSwitches()
	for _, l := range registry.All() {
		switches.SetKnown(l.ID())
	}
	// 恢复上次的设置（没提到过的车道保持默认开）。
	switches.Load(db.GetString(store.SettingLaneEnabled, ""))
	router.SetLaneSwitches(switches)

	// 恢复已存的上游凭据。
	restoreSecrets(db, vault, orLane)

	// ⚠️ 恢复限流账本与探测结论 —— 不恢复的话，重启会造成误判
	//   （见 restoreState 的注释）。
	restoreState(db, book, catalog, logger)

	gateway := server.NewGateway(router, catalog, registry, keys)
	gateway.SetUsageHook(func(rec lane.UsageRecord) {
		if err := db.PutUsage(rec); err != nil {
			logger.Printf("记用量失败: %v", err)
		}
	})

	admin := server.NewAdmin(keys, registry, catalog, db, vault)
	admin.SetLogger(logger)
	admin.SetLaneSwitches(switches)
	// ⚠️ **管理台密码** —— 初始 `admin`，首次登录后强制改。
	admin.SetPassword(password)

	srv := server.NewServer(server.Options{
		Addr:    addr,
		DataDir: dataDir,
		Log:     logger,
	}, gateway, keys)
	srv.SetAdmin(admin)

	applySettings(db, router, logger)
	applyLanTier(db, gateway)

	// ⚠️ **自动探测要花钱** —— 每一次都是一次真实推理。
	//   所以它按需探、轮转探、失败退避，而不是"每 N 分钟把全部模型探一遍"。
	//   详见 internal/lane/probescheduler.go 的文件头。
	// ⚠️ 它必须在**配置钩子之前**创建 —— 钩子要能立即调整它的间隔/批量/开关。
	prober := lane.NewProbeScheduler(catalog, registry, lane.ProbeOptions{
		Interval:    probeInterval(db),
		BatchSize:   probeBatchSize(db),
		Concurrency: 2,
	})
	prober.SetResultHook(func(r lane.ProbeResult) {
		// ⚠️ **逐模型日志** ——
		//
		//   排障最需要的是"**这个模型为什么失败**"和"**花了多久**"。
		//
		//   ⚠️ **耗时是关键的鉴别信号** ——
		//
		//	正常探测一个模型要几秒到几十秒（要真的推理）。
		//	如果整轮 27 个模型**几秒就跑完**，
		//	说明请求**根本没发出去** —— 是本地就失败了
		//	（证书读不到、DNS 不通、代理配置错）。
		//
		//	这个信号比任何错误文案都直接。
		logger.Printf("探测 %s → %s（%dms）%s",
			r.Model, r.State, r.LatencyMs, detailSuffix(r.Detail))

		// ⚠️ 写回模型行由调度器自己做了（`ApplyProbe`）——
		//   这里只负责**落盘**，重启后不用重新探一遍。
		if err := db.PutProbe(store.ProbeRow{
			Model:    r.Model,
			State:    string(r.State),
			Detail:   r.Detail,
			Latency:  r.LatencyMs,
			TTFT:     r.TTFTMs,
			ProbedAt: r.At,
		}); err != nil {
			logger.Printf("存探测结论失败: %v", err)
		}
	})
	// ⚠️ **轮次小结** ——
	//
	//	"探测完成" 没有信息量。
	//	这个小结把**耗时**和**共性失败原因**说出来：
	//
	//	耗时极短（27 个模型总共几秒）
	//	  → 请求**根本没发出去** —— 本地就失败了
	//	  → 典型的就是证书读不到 / DNS 不通
	prober.SetRoundHook(func(s lane.ProbeRoundSummary) {
		// ⚠️ **必须说清楚成功/失败** ——
		//   只说"8 个失败"会让人以为全挂了。
		msg := fmt.Sprintf("探测第 %d 轮结束：%d 个模型（可用 %d、限流 %d），共 %dms",
			s.Round, s.Probed, s.Available, s.ThrottledCount, s.DurationMs)
		if s.CommonFailure != "" {
			msg += fmt.Sprintf("；最常见失败（%d 个）：%s", s.CommonCount, s.CommonFailure)
		}
		logger.Printf("%s", msg)
	})

	// 管理台的"手动探测"按钮用同一个调度器。
	admin.SetProber(prober)
	// 限流账本 —— 管理台要用它显示冷却倒计时。
	admin.SetThrottle(book)

	// ⚠️ **上游合规监视器** ——
	// ⚠️ **出口探测** ——
	//
	// 免密车道的可用性是**网络路径的属性**，不是账号的属性：
	// 上游的原话是 "detected abusive traffic from **this IP address**"。
	//
	// 所以面板必须能回答"我现在从哪个 IP 出去" ——
	// 否则用户看到"出口被风控"这个红字，却不知道该换什么。
	//
	// ⚠️ 出口**变了**要立刻重探 —— 因为能用的模型集合跟着出口变。
	//   （换了 VPN / 代理就是换了一个出口。）
	egress := lane.NewEgressDetector()
	egress.SetOnChange(func(old, now lane.Egress) {
		logger.Printf("出口变了：%s → %s%s —— 重新探测模型可用性",
			old.IP, now.IP, countrySuffix(now.Country))
		// ⚠️ 强制重探 —— 旧的结论是**旧出口**的结论。
		prober.RunOnceManual(context.WithoutCancel(context.Background()))
	})
	admin.SetEgress(egress)
	// ⚠️ 启动后**异步**探一次 —— 不能挡住启动。
	go func() {
		e := egress.Detect(context.Background())
		if e.IP == "" {
			logger.Printf("出口检测失败：%s（不影响使用）", e.Err)
			return
		}
		logger.Printf("当前出口：%s%s", e.IP, countrySuffix(e.Country))
	}()

	// ⚠️ **配置变更立即生效** —— 没有这个钩子，管理台改了设置只写数据库，
	//   运行时不变，用户会看到"改了没效果"，直到重启。
	admin.SetConfigHook(func() {
		applySettings(db, router, logger)
		applyLanTier(db, gateway)
		// ⚠️ 探测相关设置也要**立即生效** —— 否则"探测间隔"改了要重启才变。
		prober.SetEnabled(db.GetBool(store.SettingProbeEnabled, true))
		prober.SetInterval(probeInterval(db))
		prober.SetBatchSize(probeBatchSize(db))
		// ⚠️ 自适应批量。
		prober.SetAdaptiveBatch(db.GetBool(store.SettingAdaptiveProbe, true))
	})

	// ⑫ 后台任务。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go background(ctx, logger, db, vault, registry, catalog, router, admin)

	// ⚠️ 启动自动探测（**总是启动**）——
	//   开关由调度器自己判断（`SetEnabled`），这样在管理台关掉/打开能**立即生效**，
	//   不需要重启。如果只在开启时才启动 goroutine，那"关掉再打开"就完全没反应了。
	prober.SetEnabled(db.GetBool(store.SettingProbeEnabled, true))
	prober.SetInterval(probeInterval(db))
	prober.SetBatchSize(probeBatchSize(db))
	prober.Start(ctx)
	if prober.Enabled() {
		logger.Printf("自动探测：已开启（每 %d 分钟**全量**探一遍免费模型）",
			int(probeInterval(db).Minutes()))
	} else {
		logger.Printf("自动探测：已关闭")
	}

	// ⑫ 监听。
	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	logger.Print(srv.Banner())

	// ⑬ 优雅关闭。
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		logger.Printf("收到 %s，正在关闭…", sig)
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// probeInterval 返回探测间隔。
// ⚠️ 默认 15 分钟 —— 一轮 = 一次**全量快照**（每个模型都探一次）。
//
// ⚠️ 间隔必须 **> ProbeTTL(10 分钟)** ——
//
//	否则每轮开始时，所有模型的结论都还是"新鲜的"，
//	于是**一个都不会被探**（探测形同虚设）。
func probeInterval(db *store.DB) time.Duration {
	min := db.GetInt(store.SettingProbeIntervalMin, 15)
	if min < 1 {
		min = 1
	}
	return time.Duration(min) * time.Minute
}

// probeBatchSize 返回每轮探测的模型数。
//
// ⚠️ **恒等于池子大小** —— 探测是**全量**的，每 15 分钟把
//
//	每个免费模型都探一遍。
//
//	⚠️ 之前这里有个"每轮探几个"的上限（默认 4）——
//	   那会让"全量探测"名存实亡：
//	   池子有 28 个模型，每轮只探 4 个 → 要 7 轮（1 小时 45 分）才轮完一圈。
//
//	   而且**用用户自己 Key 的车道已经不参与自动探测**了
//	   （见 OpenRouterLane.AutoProbeable），
//	   所以全量探测只花**匿名共享池**的额度 —— 不花用户的钱。
func probeBatchSize(db *store.DB) int {
	// ⚠️ 设成上限（100）而不是定值 ——
	//	池子可能超过 100 个模型（Kilo + 免密加起来将来可能上百）。
	//	这个数字只是"别无限大"的保险丝。
	return 100
}

// applyLanTier 把持久化的局域网档位应用到网关。
//
// ⚠️ **兼容旧数据** —— 老数据库只有 `lan_enabled` 布尔值，
//
//	没有 `lan_tier` 的要按 true→全开 / false→最紧的一档 迁移。
func applyLanTier(db *store.DB, gateway *server.Gateway) {
	tier := server.LanTierStandard
	if raw, ok, err := db.GetSetting(store.SettingLanTier); err == nil && ok && raw != "" {
		tier = server.ParseLanTier(raw)
	} else if db.GetBool(store.SettingLanEnabled, true) {
		tier = server.LanTierDeep
	}
	// ⚠️ 限额用**档位的默认值**，除非用户自己存过。
	defPerMin, defConc := server.TierLimits(tier)
	gateway.SetLanTier(
		tier,
		db.GetInt(store.SettingLanPerMinute, defPerMin),
		db.GetInt(store.SettingLanMaxConcurrent, defConc),
	)
}

// restoreSecrets 恢复已存的上游凭据。
// ⚠️ 解不开就跳过 —— 不该让一个坏密文阻止服务启动。管理台会显示"未配置"。
func restoreSecrets(db *store.DB, vault *store.Vault, orLane *lane.OpenRouterLane) {
	sealed, _, found, err := db.GetSecret(store.SettingSecretOpenRouter)
	if err != nil || !found {
		return
	}
	value, err := vault.Open(sealed)
	if err != nil {
		return
	}
	orLane.SetAPIKey(value)
}

// restoreState 恢复限流账本与探测结论。
// ⚠️ **必须恢复**，否则重启会造成两种误判：
// ① 限流账本丢失 → 刚被限流的模型立刻又被试一次 → 再被限流 → 白白浪费一轮候选
// ② 探测结论丢失 → 所有模型都变成"未知" → 候选链的排序失去依据
// 限流窗口尤其重要：冷却是**基于时间**的，重启后冷却应该继续，而不是从头开始。
func restoreState(db *store.DB, book *lane.Book, catalog *lane.Catalog, logger *log.Logger) {
	rows, err := db.LoadThrottle()
	if err != nil {
		logger.Printf("读限流账本失败: %v", err)
	} else if len(rows) > 0 {
		notes := map[string]lane.Note{}
		for model, row := range rows {
			note := lane.Note{
				ThrottledAt:   row.ThrottledAt,
				CooldownUntil: row.CooldownUntil,
				LastOK:        row.LastOK,
			}
			// 窗口历史是 JSON 数组。
			if row.Episodes != "" {
				_ = jsonUnmarshalString(row.Episodes, &note.Episodes)
			}
			notes[model] = note
		}
		book.Load(notes)
		logger.Printf("恢复限流账本 %d 条", len(notes))
	}

	probes, err := db.LoadProbes()
	if err != nil {
		logger.Printf("读探测结论失败: %v", err)
		return
	}
	for _, row := range probes {
		catalog.SetProbe(lane.ProbeResult{
			Model:     row.Model,
			State:     lane.ProbeState(row.State),
			Detail:    row.Detail,
			LatencyMs: row.Latency,
			TTFTMs:    row.TTFT,
			At:        row.ProbedAt,
		})
	}
	if len(probes) > 0 {
		logger.Printf("恢复探测结论 %d 条", len(probes))
	}
}

// applySettings 把持久化的设置应用到运行时。
// ⚠️ 这个函数**必须**在每次配置变更后重新调用 —— 管理台改了设置，
// 如果只写数据库不重读运行时，用户会看到"设置改了但没效果"，直到重启。
func applySettings(db *store.DB, router *lane.Router, logger *log.Logger) {
	// ⚠️ 用 `GetAutoModelSwitch` 而不是 `GetBool` ——
	//   它会把旧键 `cross_lane_fallback` 的设置迁到新键，
	//   免得升级之后开关自己变回关闭。
	switchOn := db.GetAutoModelSwitch(false)
	router.SetAutoModelSwitch(switchOn)
	if switchOn {
		logger.Printf("自动跨模型调用：已开启（模型失败时自动换：Kilo ↔ Zen → OpenRouter）")
	} else {
		logger.Printf("自动跨模型调用：已关闭（只重试当前模型，不换模型）")
	}
}

// background 是后台任务循环。
// 职责：① 定期刷新模型清单 ② 定期把限流账本落盘。
// ⚠️ **探测不在这个循环里** —— 它有自己的调度器（lane.ProbeScheduler），
// 因为探测的节奏（要避开限流、要退避、要轮转）跟"每 N 分钟做一次"不是一回事。
func background(
	ctx context.Context,
	logger *log.Logger,
	db *store.DB,
	vault *store.Vault,
	registry *lane.Registry,
	catalog *lane.Catalog,
	router *lane.Router,
	admin *server.Admin,
) {
	// 启动后先拉一次清单。
	refreshCatalog(ctx, logger, registry, catalog, db)
	// ⚠️ 清单拉取时，`Catalog.Replace/Merge` 会**自动**把探测结论
	//   套回模型行 —— 所以这里不需要额外做什么。

	refreshEvery := 30 * time.Minute
	ticker := time.NewTicker(refreshEvery)
	defer ticker.Stop()

	// 限流账本落盘。
	saveEvery := 2 * time.Minute
	saveTicker := time.NewTicker(saveEvery)
	defer saveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCatalog(ctx, logger, registry, catalog, db)
		case <-saveTicker.C:
			saveThrottle(router, db, logger)
		}
	}
}

// refreshCatalog 刷新模型清单。
func refreshCatalog(ctx context.Context, logger *log.Logger, registry *lane.Registry, catalog *lane.Catalog, db *store.DB) {
	for _, l := range registry.All() {
		// ⚠️ **额度要单独查** —— OpenRouter 的额度在 `/api/v1/key`，
		//   不在清单接口里。不查的话界面上"额度"那一栏永远是空的，
		//   用户根本不知道自己还剩多少。
		if or, ok := l.(*lane.OpenRouterLane); ok && or.Ready() {
			if q, qerr := or.FetchQuota(ctx); qerr != nil {
				logger.Printf("[%s] 查额度失败: %s", l.ID(), qerr.Message)
			} else if q != nil {
				logger.Printf("[%s] 额度: 已用 $%.4f / 上限 $%.2f",
					l.ID(), q.Usage, q.Limit)
			}
		}

		models, uerr := l.ListModels(ctx)
		if uerr != nil {
			logger.Printf("[%s] 拉清单失败: %s", l.ID(), uerr.Message)
		}
		// ⚠️ **拉取失败也要把清单写进去** ——
		//
		//   免密车道的 `ListModels` 在拉取失败时返回**快照兜底**
		//   （zen.go 里明说"一次失败的拉取绝不能让清单变小"）。
		//   但这里原来直接 `continue` —— 把兜底清单**一起丢了**，
		//   结果就是"上游一挂，清单就全空"。
		//
		//   上游不可用 ≠ 没有免费模型 —— 这两件事必须分开。
		if len(models) == 0 {
			continue
		}
		if l.ID() == lane.ZenLaneID {
			// 免密车道的清单要**替换**（它可能缩小）。
			catalog.Replace(models)
		} else {
			// OpenRouter 的清单要**合并**（免密车道的模型也在清单里）。
			catalog.Merge(models)
		}
		if uerr != nil {
			// 兜底来的 —— 说清楚，别让人以为是实时拉到的。
			logger.Printf("[%s] 清单 %d 个模型（兜底快照，上游不可用）", l.ID(), len(models))
		} else {
			logger.Printf("[%s] 清单 %d 个模型", l.ID(), len(models))
		}
	}
	// 应用用户对模型的开关。
	applyToggles(catalog, db)
}

// applyToggles 应用用户对模型的启停。
func applyToggles(catalog *lane.Catalog, db *store.DB) {
	toggles, err := db.LoadModelToggles()
	if err != nil {
		return
	}
	for _, m := range catalog.Models() {
		if enabled, ok := toggles[m.ID]; ok {
			m.Enabled = enabled
		}
	}
}

// saveThrottle 把限流账本落盘。
func saveThrottle(router *lane.Router, db *store.DB, logger *log.Logger) {
	// ⚠️ 账本在 lane.Book 里 —— 通过 router 拿不到，
	// 所以这里用 router 的导出接口。
	for _, row := range router.ThrottleRows() {
		if err := db.PutThrottle(row); err != nil {
			logger.Printf("存限流账本失败: %v", err)
		}
	}
}

// runShowKey 打印完整的 Key —— 用来解开「首次部署怎么登录」的死锁。
// ⚠️ **为什么需要它**：
// 启动日志只打**指纹**（`fmh-ca6e…e63a`）—— 这是有意的安全设计
// （日志会被收进日志系统、贴到工单里）。
// 但管理台登录要**完整** Key，而完整 Key 只在 `/api/keys?reveal=1` 里，
// 那又**需要先登录** —— 于是形成死结：
//
//	启动日志 → 只有指纹 → 要登录 → 需要完整 Key → 完整 Key 在管理台 → 要登录…
//
// 这个子命令**只读**数据目录，不需要启动服务，
// 所以可以在容器里直接跑：
//
//	docker exec free-model-house /fmh key lan
//
// ⚠️ **安全权衡**：它会打印明文 Key ——
//
//	所以它**要求能访问数据目录**（容器内 root，或宿主机的 data/ 目录）。
//	能碰到数据目录的人本来就能读到数据库里的明文 Key，
//	所以这**没有扩大**攻击面。
func runShowKey(dataDir, which string) int {
	db, err := store.Open(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		return 1
	}
	defer db.Close()

	keys := server.NewKeys(db)
	switch which {
	case "", "lan":
		fmt.Println(keys.LanKey())
	case "both":
		fmt.Println(keys.LanKey())
	default:
		fmt.Fprintf(os.Stderr, "用法: fmh key [lan]\n")
		return 1
	}
	return 0
}

// showKey 决定日志里打完整 Key 还是指纹。
// ⚠️ **默认打完整 Key** —— 因为启动日志是唯一可靠的取 Key 途径：
//   - 管理台要登录才能看 Key，登录要 Key（鸡生蛋）
//   - `docker exec free-model-house /fmh key` 需要容器**正在运行**；
//     容器起不来时（比如 NAS 上那个 `unable to find user root`），
//     这条路是断的 —— 唯一能看到的就是启动日志。
//
// ⚠️ 想保守一点：设 `FMH_HIDE_KEYS=1` 就只打指纹。
func showKey(key string) string {
	if os.Getenv("FMH_HIDE_KEYS") == "1" {
		return store.CredentialHint(key)
	}
	return key
}

// countrySuffix 给国家码加个后缀（"（中国）"）。
func countrySuffix(cc string) string {
	if cc == "" {
		return ""
	}
	return "（" + cc + "）"
}

// runHealthcheck 是容器健康检查。
func runHealthcheck(addr string) int {
	url := "http://127.0.0.1" + normalizeAddr(addr) + "/healthz"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "健康检查失败: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "健康检查返回 %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

// normalizeAddr 把 ":8678" 变成 "127.0.0.1:8678"。
func normalizeAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

// envOr 读环境变量，没有就用默认值。
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// jsonUnmarshalString 解析一个 JSON 字符串。
func jsonUnmarshalString(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}
