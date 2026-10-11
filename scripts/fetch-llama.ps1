# Downloads the pinned llama.cpp Windows builds (CPU + Vulkan) into third_party\llama\<variant>\
# and verifies their SHA-256. Safe to re-run: verified downloads are skipped.
$ErrorActionPreference = 'Stop'

$Tag = 'b11552'
$Builds = @(
    @{ Variant = 'cpu';    File = "llama-$Tag-bin-win-cpu-x64.zip";    Size = 19528406; Sha256 = 'c00741179811ff903b515eae28bdeef1f47af6e81c49920c4e68b62954e056d6' },
    @{ Variant = 'vulkan'; File = "llama-$Tag-bin-win-vulkan-x64.zip"; Size = 33473578; Sha256 = '83dc16325b532ba603034719e8377e28ca5093c5d4370141a6522f1a0250386c' }
)

$Root = Join-Path $PSScriptRoot '..\third_party\llama' | ForEach-Object { [IO.Path]::GetFullPath($_) }
New-Item -ItemType Directory -Force $Root | Out-Null

foreach ($b in $Builds) {
    $zip  = Join-Path $Root $b.File
    $dest = Join-Path $Root $b.Variant
    $url  = "https://github.com/ggml-org/llama.cpp/releases/download/$Tag/$($b.File)"

    if (Test-Path (Join-Path $dest 'llama-server.exe')) {
        Write-Host "[$($b.Variant)] already present, skipping"
        continue
    }
    if (-not (Test-Path $zip) -or (Get-Item $zip).Length -ne $b.Size) {
        Write-Host "[$($b.Variant)] downloading $url"
        Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing
    }
    $hash = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
    if ($hash -ne $b.Sha256) {
        Remove-Item $zip -Force
        throw "[$($b.Variant)] SHA-256 mismatch: got $hash, want $($b.Sha256)"
    }
    Expand-Archive -Path $zip -DestinationPath $dest -Force
    Remove-Item $zip -Force
    Write-Host "[$($b.Variant)] ok -> $dest"
}
