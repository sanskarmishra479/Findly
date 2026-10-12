# Downloads the pinned llama.cpp Windows builds (CPU + Vulkan) into third_party\llama\<variant>\
# and verifies their SHA-256. Safe to re-run: verified installs are skipped.
$ErrorActionPreference = 'Stop'

$Tag = 'b11552'
$Builds = @(
    @{ Variant = 'cpu';    File = "llama-$Tag-bin-win-cpu-x64.zip";    Size = 19528406; Sha256 = 'c00741179811ff903b515eae28bdeef1f47af6e81c49920c4e68b62954e056d6' },
    @{ Variant = 'vulkan'; File = "llama-$Tag-bin-win-vulkan-x64.zip"; Size = 33473578; Sha256 = '83dc16325b532ba603034719e8377e28ca5093c5d4370141a6522f1a0250386c' }
)

$Root = Join-Path $PSScriptRoot '..\third_party\llama' | ForEach-Object { [IO.Path]::GetFullPath($_) }
New-Item -ItemType Directory -Force $Root | Out-Null

foreach ($b in $Builds) {
    $zip    = Join-Path $Root $b.File
    $dest   = Join-Path $Root $b.Variant
    $marker = Join-Path $dest '.verified'
    $url    = "https://github.com/ggml-org/llama.cpp/releases/download/$Tag/$($b.File)"

    # The marker holds the hash of the zip the folder was extracted from. It is written
    # last, so an interrupted extract (or an older build) is never mistaken for a good one.
    if ((Test-Path $marker) -and ((Get-Content $marker -Raw).Trim() -eq $b.Sha256)) {
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
    # Extract next to the target, then swap it in, so $dest is either the old install or
    # a complete new one.
    $partial = "$dest.partial"
    if (Test-Path $partial) { Remove-Item $partial -Recurse -Force }
    Expand-Archive -Path $zip -DestinationPath $partial
    Set-Content -Path (Join-Path $partial '.verified') -Value $b.Sha256 -Encoding ascii
    if (Test-Path $dest) { Remove-Item $dest -Recurse -Force }
    Move-Item $partial $dest
    Remove-Item $zip -Force
    Write-Host "[$($b.Variant)] ok -> $dest"
}
