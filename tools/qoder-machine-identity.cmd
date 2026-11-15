@echo off
rem ==========================================================================
rem Qoder machine identity generator for FMH -- ONE file, double-click to run.
rem
rem The PowerShell code lives at the bottom of this same file, after a marker
rem line. cmd never reads past the end of this header, so both halves coexist:
rem   * double-clicking a bare .ps1 only opens Notepad, and a downloaded .ps1
rem     is additionally blocked by the default Restricted script policy -- so
rem     the entry point is a .cmd that re-launches PowerShell with
rem     -ExecutionPolicy Bypass and hands it everything below the marker;
rem   * the marker is built as '#PS' + '>' so this header line cannot match it.
rem
rem Keep everything above the marker pure ASCII with CRLF line endings: cmd
rem tracks line offsets in bytes, so UTF-8 text or LF endings in the batch
rem part make it glue two lines together and die with "is not recognized as an
rem internal or external command".
rem ==========================================================================
setlocal
rem Switch the console to UTF-8 first: the script below prints Chinese, and on a
for /f "tokens=2 delims=:." %%i in ('chcp') do set "OLDCP=%%i"
rem non-Chinese code page it comes out as mojibake. Restored right before exiting.
chcp 65001 >nul
set "SELF=%~f0"
set "PS=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
rem Windows PowerShell is only "available on demand" in future Windows
rem releases, so fall back to pwsh instead of dying with "file not found".
if not exist "%PS%" set "PS=pwsh"
rem Double-click means no arguments, and that is the only case where the shell
rem adds -Reveal on its own. Once anyone passes any argument the shell stays out
rem of it: PowerShell refuses to bind the same switch twice (ParameterAlreadyBound),
rem so a hardcoded -Reveal would make "-Reveal:$false" impossible to pass.
set "EXTRA="
if "%~1"=="" set "EXTRA=-Reveal"
"%PS%" -NoProfile -ExecutionPolicy Bypass -Command "$t=[IO.File]::ReadAllText($env:SELF);$m='#PS'+'>';$i=$t.IndexOf($m);if($i -lt 0){[Console]::Error.WriteLine('marker missing in ' + $env:SELF);exit 2};$s=[ScriptBlock]::Create($t.Substring($i + $m.Length));& $s %EXTRA% %*"
set "RC=%ERRORLEVEL%"
if defined OLDCP chcp %OLDCP% >nul
echo.
rem Double-clicked, this window would vanish before anyone reads a word of it.
pause
endlocal & exit /b %RC%

# Qoder 设备身份生成器 —— FMH「每日签到」要用的那对请求头，就这一个文件
#
# 干什么：调本机 Qoder 客户端自带的那个生成器跑一次，取回 Cosy-MachineToken /
#         Cosy-MachineType 这一对设备身份，写成网关能导入的文件。
# 为什么要有：上游在缺这一对请求头时不会下发「每日签到」这项活动（campaigns 只回
#         VIEW_DETAILS），所以领不到时没有错误码 —— 症状是「点了没反应、余额不涨」，
#         而不是「失败」。
# 为什么是设备级的：这对值是客户端在本机算出来的，OAuth / PAT / 设备码 / refresh
#         全链路都不返回它。换一台没装客户端的机器部署网关，就只能在网关那边导入一次；
#         一份身份对同一台电脑上的所有 Qoder 账号都有效，所以 FMH 存的是全局一份。
#
# 用法：双击本文件就行 —— 在装有 Qoder 客户端的那台电脑上双击，默认在桌面覆盖生成
#       qoder-machine-identity.json，然后到 FMH 的 Credit 页「设备身份」→「导入」选它。
#       命令行加开关（参数原样传给下半段的 PowerShell）：
#         -OutFile <路径>     身份文件写哪儿；留空 = 桌面，相对路径按当前目录解析。
#         -NoFile             一个文件都不生成（只想直接推网关时用）。
#         -PrintJson          顺带打印同一份 JSON，可直接粘进 FMH 的导入框。
#         -Gateway <地址>     可选：另外再 POST 一份给网关，例 http://192.168.1.20:8080。
#                             这条走明文 HTTP 且要把管理台密码交给脚本，默认不用它。
#         -Exe <路径>         手动指到 runtime-info.exe，跳过自动查找。
#         -Password '<密码>'  管理台密码，仅 -Gateway 时用；留空则隐藏输入，不进命令行历史。
#       -Reveal             写完打开资源管理器并选中那个文件。双击（一个参数都不给）时壳会
#                           自己加上它；一旦你给了任何参数，壳就不再插手，想要自己写 ——
#                           PowerShell 不允许同名开关绑两次，硬塞会让 -Reveal:$false 直接报错。
#       Windows 双击 .ps1 只会用记事本打开，默认脚本策略也拦下载来的 .ps1，所以入口做成
#       .cmd：上半段是批处理壳，标记行之后的下半段才是 PowerShell 正文。两半在同一个文件里，
#       发给别人只需要这一个文件，它自己把正文交给 powershell 跑。
#       ⚠️ 壳那半（标记行之前）必须保持纯 ASCII + CRLF：cmd 按字节偏移找行界，批处理部分
#       混进中文或换成 LF，它会把两行粘成一行，报「不是内部或外部命令」。正文随便中文。
#       这个生成器只读不写：除了身份文件，不碰客户端配置、不碰凭据文件。
#
#   如果 Credit 页里「每日签到」又不见了、或签到报「上游未确认领取成功」，
#   重跑一次脚本再导入即可（桌面那份每次被覆盖，网关也只留最新一份，不用手工清）。

#PS>
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
