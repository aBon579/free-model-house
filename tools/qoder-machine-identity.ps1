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
# 用法（默认就是"生成一个文件"，不用加参数）：
#   .\qoder-machine-identity.ps1
#     → 在脚本同目录覆盖写 qoder-machine-identity.json，然后到 FMH 的 Credit 页
#       「Qoder 设备身份」→「导入」→ 选这个文件，点保存。浏览器直接读本地文件，
#       不经过网络，也不用把管理台密码打到命令行里。
#   存到别处：  .\qoder-machine-identity.ps1 -OutFile D:\tmp\qoder.json
#   只看结果：  .\qoder-machine-identity.ps1 -NoFile -PrintJson
#   懒得开弹窗：.\qoder-machine-identity.ps1 -Gateway http://192.168.1.20:8080   # 要输密码，走明文 HTTP
#   指定 exe：  .\qoder-machine-identity.ps1 -Exe "E:\Qoder\resources\umid\runtime-info.exe"
#
# 什么时候要重跑：这两值是每次现生成的，客户端升级或过一段时间可能换成另一份；
#   如果 Credit 页里「每日签到」又不见了、或签到报「上游未确认领取成功」，
#   重跑一次脚本再导入即可（文件每次被覆盖，网关也只留最新一份，不用手工清）。
#
# 用 -Gateway 时密码默认交互输入（不落命令行历史）。要非交互就加 -Password 'xxx'。

[CmdletBinding()]
param(
    [string]$OutFile,                       # 身份文件写到哪；留空 = 脚本同目录 qoder-machine-identity.json
    [switch]$NoFile,                        # 一个文件都不生成（只想直接推网关时用）
    [switch]$PrintJson,                     # 顺带打印可直接粘进 FMH「导入设备身份」的 JSON
    [string]$Gateway,                       # 例：http://192.168.1.20:8080 ；给了才推送，否则只生成文件
    [string]$Exe,                           # runtime-info.exe 路径，留空则自动找
    [string]$Password                       # 管理台密码；仅 -Gateway 时用，留空则安全提示输入
)

$ErrorActionPreference = 'Stop'

function Find-RuntimeInfo {
    param([string]$Hint)
    if ($Hint) {
        if (-not (Test-Path -LiteralPath $Hint)) { throw "指定的文件不存在：$Hint" }
        return (Resolve-Path -LiteralPath $Hint).Path
    }
    $roots = @()
    if ($env:USERPROFILE) { $roots += (Join-Path $env:USERPROFILE '.qoder\.bin') }
    $home2 = if ($env:HOME) { $env:HOME } else { $env:USERPROFILE }
    if ($home2) { $roots += (Join-Path $home2 '.qoder/.bin') }
    $search = @()
    foreach ($r in $roots) {
        if (Test-Path -LiteralPath $r) {
            $search += Get-ChildItem -LiteralPath $r -Directory -Filter 'umid-*' -ErrorAction SilentlyContinue |
                ForEach-Object { Join-Path $_.FullName 'runtime-info.exe' }
        }
    }
    # 客户端安装目录里也有一份（本机上实测存在）
    $installDirs = @()
    foreach ($base in @($env:LOCALAPPDATA, ${env:ProgramFiles}, ${env:ProgramFiles(x86)}, 'C:\', 'D:\', 'E:\')) {
        if (-not $base) { continue }
        foreach ($name in @('Qoder', 'Qoder CN')) {
            $installDirs += (Join-Path $base (Join-Path $name 'resources\umid\runtime-info.exe'))
            $installDirs += (Join-Path $base (Join-Path $name 'resources\umid\runtime-info'))
        }
    }
    $search += $installDirs
    foreach ($cand in $search) {
        if ($cand -and (Test-Path -LiteralPath $cand)) { return (Resolve-Path -LiteralPath $cand).Path }
    }
    throw "没找到 runtime-info.exe。在装有 Qoder 客户端的机器上它通常在 `~\.qoder\.bin\umid-win32-x64-<hash>\` 或安装目录 resources\umid\ 下；也可以用 -Exe 直接指路径。"
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

if (-not $NoFile) {
    $dir = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
    $target = if ($OutFile) { $OutFile } else { Join-Path $dir 'qoder-machine-identity.json' }
    if (-not [IO.Path]::IsPathRooted($target)) { $target = Join-Path (Get-Location).Path $target }
    $parent = Split-Path -Parent $target
    if ($parent -and -not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
    # 显式 UTF-8 无 BOM：Set-Content -Encoding UTF8 会在文件头留一个 BOM，页面虽然能剥掉，
    # 但别的 JSON 读法会当场解析失败。
    [IO.File]::WriteAllText($target, $payload, (New-Object Text.UTF8Encoding($false)))
    Write-Host ''
    Write-Host "身份文件已生成：$target" -ForegroundColor Green
    Write-Host '下一步：FMH 的 Credit 页 →「Qoder 设备身份」→「导入」→ 选这个文件 → 保存。'
    Write-Host '浏览器直接读本地文件，不经过网络；这个文件里是原值，请当凭据一样保管。' -ForegroundColor Yellow
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
