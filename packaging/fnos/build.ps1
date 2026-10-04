param(
    [string]$FnpackPath = 'G:\projects\CDIfn\tools\fnpack.exe',
    [ValidateSet('amd64', 'arm64')][string]$Arch = 'amd64'
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$package = Join-Path $PSScriptRoot 'fncluster'
$server = Join-Path $package 'app\server'
$inject = Join-Path $package 'app\inject'
$uiImages = Join-Path $package 'app\ui\images'
$manifest = Join-Path $package 'manifest'
$manifestOriginal = [IO.File]::ReadAllText($manifest)
New-Item -ItemType Directory -Force $server, $inject, $uiImages | Out-Null

function Assert-LastExit([string]$step) {
    if ($LASTEXITCODE -ne 0) { throw "$step failed with exit code $LASTEXITCODE" }
}

function New-Icon([int]$size, [string]$path) {
    Add-Type -AssemblyName System.Drawing
    $bitmap = [System.Drawing.Bitmap]::new($size, $size)
    $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
    try {
        $graphics.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
        $graphics.Clear([System.Drawing.Color]::FromArgb(20, 72, 137))
        $pen = [System.Drawing.Pen]::new([System.Drawing.Color]::White, [Math]::Max(2, $size / 25))
        $brush = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::White)
        try {
            $points = @(
                @(($size * .5), ($size * .24)),
                @(($size * .27), ($size * .68)),
                @(($size * .73), ($size * .68))
            )
            foreach ($pair in @(@(0,1), @(0,2), @(1,2))) {
                $a = $points[$pair[0]]; $b = $points[$pair[1]]
                $graphics.DrawLine($pen, [float]$a[0], [float]$a[1], [float]$b[0], [float]$b[1])
            }
            $diameter = $size * .22
            foreach ($point in $points) {
                $graphics.FillEllipse($brush, [float]($point[0] - $diameter / 2), [float]($point[1] - $diameter / 2), [float]$diameter, [float]$diameter)
            }
        } finally { $pen.Dispose(); $brush.Dispose() }
        $bitmap.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
    } finally { $graphics.Dispose(); $bitmap.Dispose() }
}

$oldCGO = $env:CGO_ENABLED
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
Push-Location $root
try {
    go vet ./...
    Assert-LastExit 'go vet'
    go test ./...
    Assert-LastExit 'go test'
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'linux'
    $env:GOARCH = $Arch
    go build -trimpath -ldflags '-s -w' -o (Join-Path $server 'fncluster') ./cmd/fncluster
    Assert-LastExit 'go build'
    Copy-Item (Join-Path $root 'internal\api\web\inject\loader.js') (Join-Path $inject 'loader.js') -Force

    New-Icon 64 (Join-Path $package 'ICON.PNG')
    New-Icon 256 (Join-Path $package 'ICON_256.PNG')
    Copy-Item (Join-Path $package 'ICON.PNG') (Join-Path $uiImages 'icon_64.png') -Force
    Copy-Item (Join-Path $package 'ICON_256.PNG') (Join-Path $uiImages 'icon_256.png') -Force

    if (-not (Test-Path -LiteralPath $FnpackPath -PathType Leaf)) { throw "fnpack not found: $FnpackPath" }
    $platform = if ($Arch -eq 'arm64') { 'arm' } else { 'x86' }
    [IO.File]::WriteAllText($manifest, [regex]::Replace($manifestOriginal, '(?m)^platform\s*=.*$', "platform              = $platform"))
    $output = Join-Path $root 'fncluster.fpk'
    if (Test-Path -LiteralPath $output) { Remove-Item -LiteralPath $output -Force }
    & $FnpackPath build -d $package
    Assert-LastExit 'fnpack'
    if (-not (Test-Path -LiteralPath $output -PathType Leaf)) { throw 'fnpack did not create fncluster.fpk' }
    $name = if ($Arch -eq 'arm64') { 'fncluster-arm.fpk' } else { 'fncluster.fpk' }
    Move-Item -LiteralPath $output -Destination (Join-Path $PSScriptRoot $name) -Force
} finally {
    [IO.File]::WriteAllText($manifest, $manifestOriginal)
    $env:CGO_ENABLED = $oldCGO
    $env:GOOS = $oldGOOS
    $env:GOARCH = $oldGOARCH
    Pop-Location
}
