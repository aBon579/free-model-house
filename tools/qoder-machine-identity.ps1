# Qoder 设备身份取值脚本（在**装了 Qoder 客户端的电脑**上跑，不是在 NAS 上）
#
# 干什么：
#   调本机 Qoder 客户端自带的 umid 生成器（runtime-info.exe），拿回这台机器真实的
#   machineToken / machineType，写成一个 JSON 文件；你把这个文件导入 FMH 网关，
#   网关把它存成全局一份。
#
# 为什么要在本机跑：这两个值是**设备指纹签名**，不是账号凭据的一部分 ——
#   网关在 NAS / 容器里没有客户端，也生成不出来；而它跟登录哪个账号无关
#   （实测同一个 exe 换个 environment 参数就会吐出另一套「备用身份」，上游只认
#   IDE 用的那套，所以第一个参数 3 不能省）。
#
# 用法（默认就是"在桌面生成一个文件"，不用加参数）：
#   .\qoder-machine-identity.ps1
#     → 桌面覆盖写 qoder-machine-identity.json，然后到 FMH 的 Credit 页
#       「Qoder 设备身份」→「导入」→ 选这个文件，点保存。
#       注意：得在**生成文件这台电脑**的浏览器里打开 FMH，选文件是本机读本地磁盘。
#   打开桌面并选中它：.\qoder-machine-identity.ps1 -Reveal
#   存到别处：  .\qoder-machine-identity.ps1 -OutFile D:\tmp\qoder.json
#   只看结果：  .\qoder-machine-identity.ps1 -NoFile -PrintJson
#   懒得开弹窗：.\qoder-machine-identity.ps1 -Gateway http://192.168.1.20:8080   # 要输密码，走明文 HTTP
#   指定 exe：  .\qoder-machine-identity.ps1 -Exe "E:\Qoder\resources\umid\runtime-info.exe"
#
# 双击这个 .ps1 不会运行（Windows 会用记事本打开它）。要么右键 → 使用 PowerShell 运行，
#   要么在 PowerShell 里 cd 到 tools 目录再执行上面那行。
#
# 什么时候要重跑：这两值是每次现生成的，客户端升级或过一段时间可能换成另一份；
#   如果 Credit 页里「每日签到」又不见了、或签到报「上游未确认领取成功」，
#   重跑一次脚本再导入即可（桌面那份每次被覆盖，网关也只留最新一份，不用手工清）。
#
# 用 -Gateway 时密码默认交互输入（不落命令行历史）。要非交互就加 -Password 'xxx'。

[CmdletBinding()]
param(
    [string]$OutFile,                       # 身份文件写到哪；留空 = 桌面 qoder-machine-identity.json
    [switch]$Reveal,                        # 写完在资源管理器里打开并选中这个文件
    [switch]$NoFile,                        # 一个文件都不生成（只想直接推网关时用）
    [switch]$PrintJson,                     # 顺带打印可直接粘进 FMH「导入设备身份」的 JSON
    [string]$Gateway,                       # 例：http://192.168.1.20:8080 ；给了才推送，否则只生成文件
    [string]$Exe,                           # runtime-info.exe 路径，留空则自动找
    [string]$Password                       # 管理台密码；仅 -Gateway 时用，留空则安全提示输入
)

$ErrorActionPreference = 'Stop'

function Get-UmidExeName { return @('runtime-info.exe', 'runtime-info') }

function Get-ClientDirName { return @('Qoder', 'Qoder CN', 'qoder') }

function Find-RuntimeInfo {
    param([string]$Hint)
    if ($Hint) {
        if (-not (Test-Path -LiteralPath $Hint)) { throw "指定的文件不存在：$Hint" }
        return (Resolve-Path -LiteralPath $Hint).Path
    }
    $exeNames = Get-UmidExeName
    $clientNames = Get-ClientDirName

    # 客户端数据目录里的那一份：~/.qoder/.bin/umid-<平台>-<架构>-<hash>/runtime-info.exe
    $dataDirs = @()
    # 循环变量别叫 $home —— PowerShell 里 HOME 是只读自动变量，赋值会直接抛 VariableNotWritable。
    foreach ($userHome in @($env:USERPROFILE, $env:HOME)) {
        if ($userHome) { $dataDirs += (Join-Path $userHome '.qoder\.bin') }
    }
    foreach ($local in @($env:LOCALAPPDATA, $env:APPDATA)) {
        if ($local) { $dataDirs += (Join-Path $local 'qoder\.bin'), (Join-Path $local 'Qoder\.bin') }
    }
    foreach ($dir in $dataDirs) {
        if (-not (Test-Path -LiteralPath $dir)) { continue }
        foreach ($sub in Get-ChildItem -LiteralPath $dir -Directory -Filter 'umid-*' -ErrorAction SilentlyContinue) {
            foreach ($exe in $exeNames) {
                $cand = Join-Path $sub.FullName $exe
                if (Test-Path -LiteralPath $cand) { return (Resolve-Path -LiteralPath $cand).Path }
            }
        }
    }

    # 第一轮只查"有标准答案"的位置：注册表与 Shell API 会给本机真实的安装目录，
    # 换一台机器、换一种界面语言都不用改这里 —— 写死 C:\ D:\ E:\ 是错的，
    # 系统盘未必是 C:，Program Files 也未必在系统盘上。
    $bases = @()
    foreach ($folder in @('ProgramFiles', 'ProgramFilesX86', 'CommonProgramFiles')) {
        # 老框架上未必有 ProgramFilesX86 这个枚举值；取不到就跳过，不能让脚本死在这。
        try { $p = [Environment]::GetFolderPath($folder) } catch { $p = $null }
        if ($p) { $bases += $p }
    }
    foreach ($p in @(${env:ProgramFiles}, ${env:ProgramFiles(x86)}, $env:LOCALAPPDATA, $env:APPDATA, $env:SystemDrive)) {
        if ($p) { $bases += $p }
    }
    foreach ($base in $bases) {
        foreach ($name in $clientNames) {
            foreach ($exe in $exeNames) {
                $cand = Join-Path $base (Join-Path $name (Join-Path 'resources\umid' $exe))
                if (Test-Path -LiteralPath $cand) { return (Resolve-Path -LiteralPath $cand).Path }
            }
        }
    }

    # 第二轮才扫盘：绿色版解压到任意目录是常见情况。限制深度、跳过光驱和可移动盘，
    # 免得插着 U 盘或挂载着 NAS 时白等；这一轮只在前面都没命中时才付出代价。
    foreach ($d in [IO.DriveInfo]::GetDrives()) {
        try {
            if (-not $d.IsReady) { continue }
            if ($d.DriveType -eq 'Removable' -or $d.DriveType -eq 'Cd' -or $d.DriveType -eq 'Ram') { continue }
            $root = $d.RootDirectory.FullName
        }
        catch { continue }
        $found = Get-ChildItem -LiteralPath $root -Directory -Filter 'Qoder*' -Depth 2 -ErrorAction SilentlyContinue |
            ForEach-Object {
                foreach ($exe in $exeNames) {
                    $cand = Join-Path $_.FullName (Join-Path 'resources\umid' $exe)
                    if (Test-Path -LiteralPath $cand) { return (Resolve-Path -LiteralPath $cand).Path }
                }
            } | Select-Object -First 1
        if ($found) { return $found }
    }

    throw "没找到 runtime-info.exe。它随 Qoder 客户端安装，通常在 <用户目录>\\.qoder\\.bin\\umid-<平台>-<架构>-<hash>\\ 或安装目录 resources\\umid\\ 下；确认这台电脑装过客户端，或用 -Exe 直接指到那个 exe。"
}

function Get-MachineIdentity {
    param([string]$Path)
    # 第一个参数 3 是 IDE 用的 environment；漏掉会拿到另一套备用身份，
    # 上游据此只下发 VIEW_DETAILS，看起来就像「这个账号没有每日活动」。
    $json = '{"account":""}' | & $Path 3 --account-stdin 2>$null
    if (-not $json) { throw "runtime-info.exe 没有输出（首次运行约 3.8 秒，可能被杀软拦了）：$Path" }
    $text = ($json | Out-String)
    $start = $text.IndexOf('{')
    if ($start -lt 0) { throw "输出里找不到 JSON：$text" }
    try { $parsed = $text.Substring($start) | ConvertFrom-Json }
    catch { throw "JSON 解析失败：$text" }
    $token = "$($parsed.machineToken)".Trim()
    $type = "$($parsed.machineType)".Trim()
    if (-not $token -or -not $type) { throw "输出里没有 machineToken / machineType：$text" }
    return [pscustomobject]@{
        machineToken  = $token
        machineType   = $type
        machineCode   = "$($parsed.machineCode)".Trim()
        accountOutcome = "$($parsed.accountOutcome)".Trim()
    }
}

function Format-Hint {
    param([string]$Value)
    if ($Value.Length -le 10) { return $Value }
    return $Value.Substring(0, 6) + '…' + $Value.Substring($Value.Length - 4)
}

function Get-IdentityFilePath {
    param([string]$Override)
    if ($Override) {
        if ([IO.Path]::IsPathRooted($Override)) { return $Override }
        return Join-Path (Get-Location).Path $Override
    }
    # 桌面必须用 Shell API 取，不能拼 $env:USERPROFILE\Desktop：OneDrive「已知文件夹重定向」、
    # 组策略改过已知文件夹、或干脆把桌面挪到别的盘的机器上，真实桌面在别处，而拼出来的那个
    # 目录往往**也存在**——文件写进去了却看不见，对使用者来说就等于没生成。
    # 也不做 Test-Path 门槛：路径合法但目录还没建时，直接建出来才是他要的那个桌面
    # （非 ASCII 的桌面名无所谓，全程走 -LiteralPath）。
    foreach ($folder in @('Desktop', 'Personal')) {
        try { $dir = [Environment]::GetFolderPath($folder) } catch { $dir = $null }
        if ($dir) { return Join-Path $dir 'qoder-machine-identity.json' }
    }
    Write-Host '没拿到桌面或文档目录，身份文件改放临时目录（重启后可能被清理，导入完就删掉它）。' -ForegroundColor Yellow
    return Join-Path $env:TEMP 'qoder-machine-identity.json'
}

function Get-IdentityJson {
    param($Identity)
    # 包在 machineIdentity 块里：FMH 页面的「导入」认这个键（裸的一对也认）。
    # generatedAt 只是给人看的——文件放久了身份可能已被客户端换掉，那时再导旧文件，
    # 症状跟没配一样：上游不下发「每日签到」。
    return ([ordered]@{
        machineIdentity = [ordered]@{ machineToken = $Identity.machineToken; machineType = $Identity.machineType }
        machineCode     = $Identity.machineCode
        generatedAt     = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    } | ConvertTo-Json -Compress)
}

$exePath = Find-RuntimeInfo -Hint $Exe
Write-Host "设备身份生成器：$exePath"
$identity = Get-MachineIdentity -Path $exePath
Write-Host ("machineToken：{0}（{1} 字符）" -f (Format-Hint $identity.machineToken), $identity.machineToken.Length)
Write-Host ("machineType ：{0}" -f $identity.machineType)
Write-Host ("machineCode ：{0}（上游不要求，仅记录）" -f $identity.machineCode)
if ($identity.accountOutcome -and $identity.accountOutcome -ne 'success') {
    Write-Host ("accountOutcome={0}：这两个值与账号无关，属正常，继续。" -f $identity.accountOutcome)
}

$payload = Get-IdentityJson -Identity $identity
$encoding = New-Object Text.UTF8Encoding($false)

if (-not $NoFile) {
    $target = Get-IdentityFilePath -Override $OutFile
    $replaced = Test-Path -LiteralPath $target
    $parent = Split-Path -Parent $target
    try {
        if ($parent -and -not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
        # 显式 UTF-8 无 BOM：Set-Content -Encoding UTF8 会在文件头留一个 BOM，页面虽然能剥掉，
        # 但别的 JSON 读法会当场解析失败。
        [IO.File]::WriteAllText($target, $payload, $encoding)
    }
    catch {
        # OneDrive 正在同步 / 桌面被组策略设成只读时写不进去；对使用者来说"文件在哪"比"为什么失败"有用，
        # 所以退到临时目录再试一次，并把两个路径都说清楚。
        $fallback = Join-Path $env:TEMP 'qoder-machine-identity.json'
        if ($target -eq $fallback) { throw "身份文件写不出来：$_" }
        [IO.File]::WriteAllText($fallback, $payload, $encoding)
        $target = $fallback
        Write-Host ''
        Write-Host "原定位置写入失败（$_），已改放临时目录。" -ForegroundColor Yellow
    }
    Write-Host ''
    Write-Host "身份文件已生成：$target" -ForegroundColor Green
    if ($replaced) { Write-Host '（覆盖的是上一次生成的那份，导入这份就够）' -ForegroundColor DarkGray }
    Write-Host '下一步：FMH 的 Credit 页 →「Qoder 设备身份」→「导入」→ 选这个文件 → 保存。'
    Write-Host '这个文件里是原值，请当凭据一样保管；导入后网关不会把它回显给你。' -ForegroundColor Yellow
    if ($Reveal) { Start-Process explorer.exe -ArgumentList ('/select,"' + $target + '"') }
}

if ($PrintJson) {
    Write-Host ''
    Write-Host '下面这段可以直接粘进 FMH 的「导入设备身份」：'
    Write-Host $payload
}

if (-not $Gateway) {
    if ($NoFile) {
        Write-Host ''
        Write-Host '加了 -NoFile 又没给 -Gateway，所以这次什么都没写。' -ForegroundColor DarkGray
    }
    return
}

if (-not $Password) {
    $secure = Read-Host -Prompt "FMH 管理台密码（$Gateway）" -AsSecureString
    $Password = [Net.NetworkCredential]::new('', $secure).Password
}

$base = $Gateway.TrimEnd('/')
$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
try {
    $loginBody = '{"password":"' + ($Password -replace '"', '\"') + '"}'
    Invoke-RestMethod -Method Post -Uri "$base/api/login" -Body $loginBody -ContentType 'application/json' -WebSession $session | Out-Null
}
catch {
    throw "登录失败：$_（密码对不对？网关是不是局域网可达？）"
}

$pushBody = '{"machineToken":"' + ($identity.machineToken -replace '"', '\"') + '","machineType":"' + ($identity.machineType -replace '"', '\"') + '"}'
try {
    $resp = Invoke-RestMethod -Method Post -Uri "$base/api/credit/machine" -Body $pushBody -ContentType 'application/json' -WebSession $session
}
catch {
    throw "推送失败：$_"
}
if (-not $resp.ok) { throw "网关没接受这次写入：$($resp | ConvertTo-Json -Compress)" }

Write-Host ''
Write-Host ("已推送到 {0}；网关现存身份：{1}" -f $base, $resp.machine.hint) -ForegroundColor Green
Write-Host '回到 Credit 页点一次「查询余额」，活动清单里应该多出「每日签到」那一项。'
