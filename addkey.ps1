# addkey.ps1 — 往 auths/ 目录添加一个 CodeBuddy 凭证（ck_ API Key）。
#
# 用法:
#   .\addkey.ps1
#   .\addkey.ps1 -Key ck_xxxx -Name 备注名
#
# 添加后在管理后台点「重载目录」，或重启服务即可生效。

param(
    [string]$Key,
    [string]$Name
)

$ErrorActionPreference = 'Stop'
$authDir = if ($env:CB2A_AUTH_DIR) { $env:CB2A_AUTH_DIR } else { Join-Path $PSScriptRoot 'auths' }
New-Item -ItemType Directory -Force -Path $authDir | Out-Null

if (-not $Key) {
    $Key = (Read-Host 'CodeBuddy API Key (ck_...)').Trim()
}
$Key = $Key -replace '\s', ''
if (-not $Key) {
    Write-Error 'API Key 不能为空'
    exit 1
}

if (-not $Name) { $Name = '' }
$Name = $Name.Trim()
if (-not $Name) {
    $sha = [System.BitConverter]::ToString(
        [System.Security.Cryptography.SHA256]::Create().ComputeHash([Text.Encoding]::UTF8.GetBytes($Key))
    ).Replace('-', '').ToLower().Substring(0, 10)
    $Name = "key-$sha"
}

$out = Join-Path $authDir ("codebuddy-$Name.json")
$doc = [ordered]@{
    api_key  = $Key
    uid      = $Name
    nickname = $Name
    type     = 'api_key'
}
$doc | ConvertTo-Json | Set-Content -Path $out -Encoding UTF8

Write-Host "已写入: $out"
Write-Host ''
Write-Host '提示：管理后台 /admin → 「重载目录」后生效。'
