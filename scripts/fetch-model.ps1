# Downloads the EmbeddingGemma 2 Q8_0 GGUF files into third_party\models\ and verifies SHA-256.
# Dev-only helper for Phase 0; the app itself uses internal/model for staged downloads.
$ErrorActionPreference = 'Stop'

$Repo = 'ggml-org/embeddinggemma-2-GGUF'
$Files = @(
    @{ File = 'embeddinggemma-2-Q8_0.gguf';        Size = 309855456; Sha256 = '2188ac1deca4b77dffefd603c2776a9d76d9d74ec01841392982ebb840b09135' },
    @{ File = 'mmproj-embeddinggemma-2-Q8_0.gguf'; Size = 554821024; Sha256 = 'c4a8a52691ecef40618438928bdf9e68379b854e24166f292592353db0aab64f' }
)

$Root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\third_party\models'))
New-Item -ItemType Directory -Force $Root | Out-Null

foreach ($f in $Files) {
    $out = Join-Path $Root $f.File
    $tmp = "$out.download"
    if ((Test-Path $out) -and (Get-Item $out).Length -eq $f.Size) {
        Write-Host "$($f.File) already present, skipping"
        continue
    }
    $url = "https://huggingface.co/$Repo/resolve/main/$($f.File)"
    Write-Host "downloading $url ($([math]::Round($f.Size / 1MB)) MB)"
    # curl.exe ships with Windows 10+; -C - resumes a partial download.
    & curl.exe -L --fail -C - -o $tmp $url
    if ($LASTEXITCODE -ne 0) { throw "download failed: $($f.File)" }
    $hash = (Get-FileHash $tmp -Algorithm SHA256).Hash.ToLower()
    if ($hash -ne $f.Sha256) {
        Remove-Item $tmp -Force
        throw "SHA-256 mismatch for $($f.File): got $hash, want $($f.Sha256)"
    }
    Move-Item $tmp $out -Force
    Write-Host "$($f.File) ok"
}
